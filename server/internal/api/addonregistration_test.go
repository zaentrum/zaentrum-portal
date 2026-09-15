package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

func TestRegistrationSteps(t *testing.T) {
	items := []operator.ChartAddon{
		{Name: "example", Phase: "Ready"},
		{Name: "sample", Phase: "Planned", Suspend: true}, // planned only: nothing to register
		{Name: "alpha", Phase: "Ready"},
		{Name: "beta", Phase: "Degraded"},               // not ready (yet, or any more): left as it is
		{Name: "going", Phase: "Ready", Deleting: true}, // a finalizer holds it: as good as gone
	}
	registered := []model.Addon{
		{Key: "example", ChartRef: "oci://registry.example.org/charts/example"},
		{Key: "beta", ChartRef: "oci://registry.example.org/charts/beta"},
		{Key: "gone", ChartRef: "oci://registry.example.org/charts/gone"},
		{Key: "legacy"}, // added by address: no resource to lose
		{Key: "going", ChartRef: "oci://registry.example.org/charts/going"},
	}
	var got []string
	for _, s := range registrationSteps(items, registered) {
		if s.unregister {
			got = append(got, "-"+s.name)
		} else {
			got = append(got, "+"+s.name)
		}
	}
	if strings.Join(got, " ") != "+alpha +example -going -gone" {
		t.Errorf("steps = %v", got)
	}
}

func TestNeedsRegistration(t *testing.T) {
	chart := operator.ChartSource{Ref: "oci://registry.example.org/charts/example", Version: "1.2.0"}
	current := &model.Addon{Key: "example", ManifestSHA256: "aa", ChartRef: chart.Ref, ChartVersion: chart.Version}
	cases := []struct {
		name  string
		reg   *model.Addon
		sha   string
		chart operator.ChartSource
		want  bool
	}{
		{"not registered", nil, "aa", chart, true},
		{"nothing changed", current, "aa", chart, false},
		{"another manifest", current, "bb", chart, true},
		{"another chart version", current, "aa", operator.ChartSource{Ref: chart.Ref, Version: "1.3.0"}, true},
		{"another chart", current, "aa", operator.ChartSource{Ref: "https://charts.example.org/example-1.2.0.tgz"}, true},
		{"a digest alone is no new source", current, "aa", operator.ChartSource{Ref: chart.Ref, Version: "1.2.0", Digest: "sha256:" + strings.Repeat("c", 64)}, false},
		{"registered by address", &model.Addon{Key: "example", ManifestSHA256: "aa"}, "aa", chart, true},
	}
	for _, c := range cases {
		if got := needsRegistration(c.reg, c.sha, c.chart); got != c.want {
			t.Errorf("%s: needsRegistration = %v, want %v", c.name, got, c.want)
		}
	}
	applied := operator.ChartSource{Ref: chart.Ref, Version: "1.1.0"}
	if got := runningChart(operator.ChartAddon{Chart: chart, LastAppliedChart: &applied}); got != applied {
		t.Errorf("the running chart is the applied one, got %+v", got)
	}
	if got := runningChart(operator.ChartAddon{Chart: chart}); got != chart {
		t.Errorf("without an applied chart, the spec's, got %+v", got)
	}
}

// chartManifest is what the example chart's primary serves: two components and
// a slot row with a portal-relative URL.
const chartManifest = `{
  "service": "example", "kind": "addon", "version": "2.0.0",
  "components": [
    {"name": "example", "workload": "example", "role": "primary", "summary": "serves the console"},
    {"name": "worker", "workload": "example-worker", "role": "optional", "summary": "processes the queue", "topics": ["example.item.done"]}
  ],
  "ui": {"app": {"title": "Example"}, "console": true,
         "slots": [{"key": "hint", "slot": "search.empty", "label": "open example", "url": "/portal/app/example?q={q}"}]}
}`

// readyStatus is the operator's status for an applied, ready example addon.
func readyStatus(e *chartEnv, version string, primary string) map[string]any {
	plan := examplePlan(version)
	plan["chart"].(map[string]any)["annotations"] = map[string]any{"zaentrum.io/addon": "true", "zaentrum.io/primary": primary}
	return map[string]any{
		"phase": "Ready", "observedGeneration": e.kube.Generation(addonPlural, "example"), "plan": plan,
		"lastAppliedChart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": version},
		"components": []any{
			map[string]any{"name": "example", "kind": "Deployment", "ready": 1, "desired": 1},
			map[string]any{"name": "example-worker", "kind": "Deployment", "ready": 1, "desired": 1},
		},
	}
}

// registerInstalls makes the in-memory registry hold what the last install
// wrote, as the store does.
func registerInstalls(f *fakeAddonStore) {
	in := f.installs[len(f.installs)-1]
	ad := in.Addon
	ad.Components = in.Components
	f.addons[ad.Key] = ad
	f.apps[in.App.Key] = in.App
	for _, c := range in.Components {
		f.claims[c.Workload] = ad.Key
	}
}

func TestSyncChartAddons(t *testing.T) {
	e := newChartEnv(t)
	e.api.workloads = fakeWorkloads{available: true} // a cluster that lists, nothing of the platform's in the way
	e.kube.Put("zaentrums", map[string]any{"metadata": map[string]any{"name": "zaentrum"}, "spec": map[string]any{"hostname": "zaentrum.example.org"}})
	served := decodeManifest(t, chartManifest)
	var fetched []string
	e.api.registration.fetch = func(_ context.Context, proxyURL string) (Descriptor, error) {
		fetched = append(fetched, proxyURL)
		return served, nil
	}
	e.seed("example", map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}}, nil)
	sync := func() { e.api.syncChartAddons(context.Background()) }

	// Installing: not ready, so nothing is read and nothing registered.
	status := readyStatus(e, "1.2.0", "example")
	status["phase"] = "Installing"
	e.kube.SetStatus(addonPlural, "example", status)
	sync()
	if len(fetched) != 0 || len(e.store.installs) != 0 {
		t.Fatalf("an addon that is not ready: fetched %v, installs %d", fetched, len(e.store.installs))
	}

	// Ready: registered from its primary, with the chart as its source.
	e.kube.SetStatus(addonPlural, "example", readyStatus(e, "1.2.0", "example"))
	sync()
	if len(fetched) != 1 || fetched[0] != "http://example" || len(e.store.installs) != 1 {
		t.Fatalf("fetched %v, installs %d", fetched, len(e.store.installs))
	}
	in := e.store.installs[0]
	if in.App.Key != "example" || in.Addon.Address != "http://example" || in.Addon.ChartRef != "oci://registry.example.org/charts/example" ||
		in.Addon.ChartVersion != "1.2.0" || len(in.Components) != 2 || len(in.Tiles) != 1 {
		t.Errorf("install = %+v", in)
	}
	if len(in.Rows) != 1 || in.Rows[0].URL != "https://zaentrum.example.org/portal/app/example?q={q}" {
		t.Errorf("slot URLs are absolutised against the platform's hostname: %+v", in.Rows)
	}
	// Components are named as the operator labels them — by Deployment — with
	// the chart's primary and every other workload required.
	wantComponents := []model.AddonComponent{
		{Name: "example", Workload: "example", Role: "primary", Summary: "serves the console"},
		{Name: "example-worker", Workload: "example-worker", Role: "required", Summary: "processes the queue", Order: 1},
	}
	if len(in.Components) != 2 || in.Components[0] != wantComponents[0] || in.Components[1] != wantComponents[1] {
		t.Errorf("components = %+v", in.Components)
	}
	registerInstalls(e.store)
	if rec := e.do(http.MethodGet, "/api/portal/addon-charts/example", nil); !strings.Contains(rec.Body.String(), `"registered":true`) {
		t.Errorf("get after registration = %s", rec.Body)
	}

	// Nothing changed: read again, nothing written.
	sync()
	if len(fetched) != 2 || len(e.store.installs) != 1 {
		t.Fatalf("unchanged: fetched %d, installs %d", len(fetched), len(e.store.installs))
	}

	// Upgraded: the new chart version is recorded.
	e.kube.SetStatus(addonPlural, "example", readyStatus(e, "1.3.0", "example"))
	sync()
	if len(e.store.installs) != 2 || e.store.installs[1].Addon.ChartVersion != "1.3.0" {
		t.Fatalf("upgrade: installs %d %+v", len(e.store.installs), e.store.installs[len(e.store.installs)-1].Addon)
	}
	registerInstalls(e.store)

	// Redeployed with another manifest: refreshed.
	served.Version = "2.1.0"
	sync()
	if len(e.store.installs) != 3 || e.store.installs[2].Addon.Version != "2.1.0" {
		t.Fatalf("new manifest: installs %d", len(e.store.installs))
	}
	registerInstalls(e.store)

	// A manifest that names another service is refused, and says so.
	served.Service = "other"
	served.Version = "2.2.0"
	sync()
	if len(e.store.installs) != 3 {
		t.Fatal("a manifest for another service must not register")
	}
	rec := e.do(http.MethodGet, "/api/portal/addon-charts/example", nil)
	if !strings.Contains(rec.Body.String(), `names service \"other\"`) {
		t.Errorf("the registration error must be shown: %s", rec.Body)
	}
	served.Service = "example"
	sync()
	if len(e.store.installs) != 4 || e.api.registrationError("example") != "" {
		t.Fatalf("fixed: installs %d, error %q", len(e.store.installs), e.api.registrationError("example"))
	}
	registerInstalls(e.store)

	// The chart must name its primary.
	e.kube.SetStatus(addonPlural, "example", readyStatus(e, "1.3.0", ""))
	sync()
	if !strings.Contains(e.api.registrationError("example"), "primary") {
		t.Errorf("error = %q", e.api.registrationError("example"))
	}

	// A list that fails unregisters nothing.
	e.kube.Unserved[addonPlural] = true
	e.kube.Remove(addonPlural, "example")
	sync()
	if len(e.store.removals) != 0 {
		t.Fatalf("a failed read must not unregister: %v", e.store.removals)
	}
	// Deleted elsewhere: its rows go.
	e.kube.Unserved[addonPlural] = false
	sync()
	if len(e.store.removals) != 1 || !strings.HasPrefix(e.store.removals[0], "example|") {
		t.Fatalf("removals = %v", e.store.removals)
	}
}

// The loop passes at once, again when kicked — well before its next tick —
// and stops with its context.
func TestRunAddonRegistration(t *testing.T) {
	e := newChartEnv(t)
	e.api.workloads = fakeWorkloads{available: true}
	var (
		mu      sync.Mutex
		fetched int
	)
	e.api.registration.fetch = func(context.Context, string) (Descriptor, error) {
		mu.Lock()
		defer mu.Unlock()
		fetched++
		return decodeManifest(t, chartManifest), nil
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return fetched
	}
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("fetched %d times, want %d", count(), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	e.seed("example", map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}}, nil)
	e.kube.SetStatus(addonPlural, "example", readyStatus(e, "1.2.0", "example"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.api.RunAddonRegistration(ctx)
		close(done)
	}()
	waitFor(1)
	e.api.kickRegistration()
	waitFor(2)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the loop must stop with its context")
	}
	// This registry never comes to hold the addon, so each pass installs it.
	if len(e.store.installs) != 2 {
		t.Errorf("installs = %d, want one per pass", len(e.store.installs))
	}

	// Outside a cluster the loop has nothing to do and returns.
	returned := make(chan struct{})
	go func() {
		(&API{addons: newFakeStore()}).RunAddonRegistration(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("outside a cluster the loop must return at once")
	}
}

func TestSyncChartAddonsRefusesToTakeOver(t *testing.T) {
	setup := func(t *testing.T) *chartEnv {
		e := newChartEnv(t)
		e.api.workloads = fakeWorkloads{available: true}
		e.api.registration.fetch = func(context.Context, string) (Descriptor, error) {
			return decodeManifest(t, chartManifest), nil
		}
		e.seed("example", map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}}, nil)
		e.kube.SetStatus(addonPlural, "example", readyStatus(e, "1.2.0", "example"))
		return e
	}
	cases := []struct {
		name    string
		prepare func(*chartEnv)
		mention string
	}{
		{"an addon added by address elsewhere", func(e *chartEnv) {
			e.store.apps["example"] = model.App{Key: "example", BaseURL: "/portal/app/example", ProxyURL: "http://example.other.svc.cluster.local"}
			e.store.addons["example"] = model.Addon{Key: "example", Address: "http://example.other.svc.cluster.local"}
		}, "replaceAddress"},
		{"a platform workload", func(e *chartEnv) {
			e.api.workloads = fakeWorkloads{available: true, instances: []operator.Instance{{Name: "example-worker", Group: "platform"}}}
		}, "platform deployment"},
		{"workloads that cannot be listed", func(e *chartEnv) {
			e.api.workloads = fakeWorkloads{available: true, err: errors.New("forbidden")}
		}, "cannot list its workloads"},
		{"an app registered by hand", func(e *chartEnv) {
			e.store.apps["example"] = model.App{Key: "example", Title: "mine"}
		}, "not installed as an addon"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			c.prepare(e)
			e.api.syncChartAddons(context.Background())
			if len(e.store.installs) != 0 || !strings.Contains(e.api.registrationError("example"), c.mention) {
				t.Fatalf("installs %d, error %q — want a refusal mentioning %q", len(e.store.installs), e.api.registrationError("example"), c.mention)
			}
		})
	}

	t.Run("an addon added by address at the same Service becomes the chart's", func(t *testing.T) {
		e := setup(t)
		e.store.apps["example"] = model.App{Key: "example", BaseURL: "/portal/app/example", ProxyURL: "http://example"}
		e.store.addons["example"] = model.Addon{Key: "example", Address: "http://example"}
		e.api.syncChartAddons(context.Background())
		if len(e.store.installs) != 1 || e.store.installs[0].Addon.ChartRef == "" {
			t.Fatalf("installs %+v, error %q", e.store.installs, e.api.registrationError("example"))
		}
	})
}

func TestListAddonsMergesChartAddons(t *testing.T) {
	e := newChartEnv(t)
	discCache.mu.Lock()
	discCache.fetched, discCache.doc, discCache.descs = time.Now(), []byte("{}"), nil
	discCache.mu.Unlock()
	t.Cleanup(func() {
		discCache.mu.Lock()
		discCache.fetched, discCache.doc, discCache.descs = time.Time{}, nil, nil
		discCache.mu.Unlock()
	})

	manifest, sum := canonicalManifest(decodeManifest(t, chartManifest))
	e.store.addons["example"] = model.Addon{
		Key: "example", Title: "Example", Address: "http://example", Version: "2.0.0", Manifest: manifest, ManifestSHA256: sum,
		ChartRef: "oci://registry.example.org/charts/example", ChartVersion: "1.2.0",
		Components: []model.AddonComponent{
			{Name: "example", Workload: "example", Role: "primary", Summary: "serves the console"},
			{Name: "example-worker", Workload: "example-worker", Role: "required", Summary: "processes the queue", Order: 1},
		},
	}
	e.store.addons["legacy"] = model.Addon{Key: "legacy", Address: "http://legacy", Components: []model.AddonComponent{{Name: "legacy", Workload: "legacy", Role: "primary"}}}

	// example: running 1.2.0, an upgrade to 1.3.0 planned; its worker crashes.
	e.seed("example", map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.3.0"}, "suspend": true}, nil)
	status := readyStatus(e, "1.2.0", "example")
	status["components"] = []any{
		map[string]any{"name": "example-worker", "kind": "Deployment", "ready": 0, "desired": 1, "reason": "CrashLoopBackOff"},
		map[string]any{"name": "example", "kind": "Deployment", "ready": 1, "desired": 1},
	}
	e.kube.SetStatus(addonPlural, "example", status)
	// sample: planned only.
	e.seed("sample", map[string]any{"chart": map[string]any{"ref": "https://charts.example.org/sample-0.1.0.tgz"}, "suspend": true}, nil)
	plan := examplePlan("0.1.0")
	plan["chart"] = map[string]any{"name": "sample", "version": "0.1.0", "annotations": map[string]any{"zaentrum.io/title": "Sample", "zaentrum.io/primary": "sample"}}
	e.kube.SetStatus(addonPlural, "sample", map[string]any{"phase": "Planned", "observedGeneration": 1, "plan": plan})

	rec := e.do(http.MethodGet, "/api/portal/addons", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var rows []installedAddon
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Key != "example" || rows[1].Key != "legacy" || rows[2].Key != "sample" {
		t.Fatalf("rows = %+v", rows)
	}
	ex, legacy, sample := rows[0], rows[1], rows[2]
	if ex.Chart == nil || ex.Chart.Version != "1.3.0" || ex.Chart.LastApplied == nil || ex.Chart.LastApplied.Version != "1.2.0" ||
		ex.Phase != "Ready" || !ex.Suspended || !ex.Registered || ex.RefreshAvailable {
		t.Errorf("example = %+v chart=%+v", ex, ex.Chart)
	}
	if len(ex.Components) != 2 {
		t.Fatalf("components = %+v", ex.Components)
	}
	primary, worker := ex.Components[0], ex.Components[1]
	if primary.Workload != "example" || primary.Role != "primary" || *primary.Phase != "ready" || primary.Summary != "serves the console" {
		t.Errorf("primary = %+v", primary)
	}
	// The chart's other workloads are required, whatever the manifest says.
	if worker.Name != "example-worker" || worker.Workload != "example-worker" || worker.Role != "required" || *worker.Phase != "degraded" ||
		*worker.Ready != 0 || *worker.Desired != 1 || *worker.Reason != "CrashLoopBackOff" || worker.Summary != "processes the queue" ||
		strings.Join(worker.Topics, ",") != "example.item.done" {
		t.Errorf("worker = %+v", worker)
	}
	if legacy.Chart != nil || !legacy.Registered || legacy.Phase != "" {
		t.Errorf("an addon added by address has no chart: %+v", legacy)
	}
	if sample.Registered || sample.Title != "Sample" || !sample.Suspended || sample.Phase != "Planned" ||
		sample.Chart == nil || sample.Chart.Ref != "https://charts.example.org/sample-0.1.0.tgz" || sample.Chart.LastApplied != nil ||
		sample.Components == nil || len(sample.Components) != 0 {
		t.Errorf("planned addon = %+v", sample)
	}
	if !strings.Contains(rec.Body.String(), `"components":[]`) {
		t.Error("an addon without components must list [] — the console tells an older portal-api by a missing list")
	}
}

// An addon installed from a chart is upgraded, reconfigured and removed as one:
// the address path must neither take it over nor strip its rows.
func TestAddressPathLeavesChartAddonsAlone(t *testing.T) {
	e := newChartEnv(t)
	e.store.apps["example"] = model.App{Key: "example", BaseURL: "/portal/app/example", ProxyURL: "http://example"}
	e.store.addons["example"] = model.Addon{Key: "example", Address: "http://example", ChartRef: "oci://registry.example.org/charts/example"}
	addr := manifestServer(t, localhostManifest)
	for _, dry := range []bool{true, false} {
		rec := postInstall(t, e.api, map[string]any{"proxyUrl": addr, "dryRun": dry})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "chart") {
			t.Errorf("install by address (dryRun=%v) = %d %s", dry, rec.Code, rec.Body)
		}
	}
	if rec := e.do(http.MethodDelete, "/api/portal/addons/example", nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "addon-charts") {
		t.Errorf("remove by key = %d %s", rec.Code, rec.Body)
	}
	if len(e.store.installs) != 0 || len(e.store.removals) != 0 {
		t.Errorf("installs %d, removals %v", len(e.store.installs), e.store.removals)
	}
}

func TestChartComponents(t *testing.T) {
	declared := []model.AddonComponent{
		{Name: "example", Workload: "example", Role: "primary", Summary: "serves the console"},
		{Name: "worker", Workload: "example-worker", Role: "optional", Summary: "processes the queue", Order: 1},
	}
	withPrimary := func(components ...operator.ChartComponent) operator.ChartAddon {
		ca := operator.ChartAddon{Name: "example", Components: components, Plan: &operator.ChartPlan{}}
		ca.Plan.Chart.Annotations = map[string]string{operator.AnnotationPrimary: "example"}
		return ca
	}
	names := func(cs []model.AddonComponent) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name+"/"+c.Role)
		}
		return strings.Join(out, " ")
	}
	// The status lists what runs, in its own order; the primary leads.
	got := chartComponents(withPrimary(
		operator.ChartComponent{Name: "example-worker"}, operator.ChartComponent{Name: "example-cache"}, operator.ChartComponent{Name: "example"},
	), declared)
	if names(got) != "example/primary example-worker/required example-cache/required" || got[1].Summary != "processes the queue" || got[2].Order != 2 {
		t.Errorf("from status = %+v", got)
	}
	// No status components yet: the manifest's, named by workload.
	if got := chartComponents(withPrimary(), declared); names(got) != "example/primary example-worker/required" {
		t.Errorf("from the manifest = %+v", got)
	}
	// A workload name that is no label cannot be a component row.
	if got := chartComponents(withPrimary(operator.ChartComponent{Name: "example.v2"}), declared); names(got) != "example/primary" {
		t.Errorf("non-label names = %+v", got)
	}
}
