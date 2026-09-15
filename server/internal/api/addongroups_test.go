package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// An addon made of two workloads that reports its own setup state. Everything
// the component-group contract adds, in one neutral manifest.
const groupManifest = `{
  "service": "example", "kind": "addon", "version": "2.0.0",
  "components": [
    {"name": "example", "workload": "example", "role": "primary", "summary": "serves the manifest and the console"},
    {"name": "worker", "workload": "example-worker", "role": "optional", "summary": "processes the queue", "topics": ["example.item.done", " "]}
  ],
  "setup": {
    "path": "/api/setup",
    "sections": [
      {"key": "policy", "title": "policy", "required": false, "target": "#/settings", "ord": 30},
      {"key": "sources", "title": "sources", "description": "where items come from", "required": true, "target": "#/sources", "ord": 10},
      {"key": "workers", "required": true, "target": "#/workers", "ord": 20}
    ]
  },
  "ui": {"app": {"title": "Example"}, "console": true}
}`

func TestPlanAddonComponentsAndSetup(t *testing.T) {
	plan, err := planAddon("http://example.zaentrum.svc.cluster.local:8080", decodeManifest(t, groupManifest), "apps", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Components) != 2 {
		t.Fatalf("components = %+v", plan.Components)
	}
	c0, c1 := plan.Components[0], plan.Components[1]
	if c0.Name != "example" || c0.Workload != "example" || c0.Role != "primary" || c0.Order != 0 {
		t.Errorf("primary = %+v", c0)
	}
	if c1.Name != "worker" || c1.Workload != "example-worker" || c1.Role != "optional" || c1.Order != 1 {
		t.Errorf("worker = %+v", c1)
	}
	if got := plan.Topics["worker"]; len(got) != 1 || got[0] != "example.item.done" {
		t.Errorf("topics = %v — blank topics are dropped", got)
	}
	if plan.Setup == nil || plan.Setup.Path != "/api/setup" || len(plan.Setup.Sections) != 3 {
		t.Fatalf("setup = %+v", plan.Setup)
	}
	// Sections come back in display order, with the title defaulted.
	var keys []string
	for _, s := range plan.Setup.Sections {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != "sources,workers,policy" {
		t.Errorf("section order = %v", keys)
	}
	if plan.Setup.Sections[1].Title != "workers" {
		t.Errorf("an untitled section is titled by its key: %+v", plan.Setup.Sections[1])
	}
	if plan.Version != "2.0.0" || len(plan.Manifest) == 0 || len(plan.SHA256) != 64 {
		t.Errorf("version/manifest/sha = %q/%d/%q", plan.Version, len(plan.Manifest), plan.SHA256)
	}
}

// A manifest without components is still a group: of exactly one, the
// workload the admin typed the address of. This is every addon written before
// components existed, and it must keep installing unchanged.
func TestPlanAddonWithoutComponents(t *testing.T) {
	cases := []struct {
		name, address, service, workload string
	}{
		{"bare service name", "http://sample-addon", "sample", "sample-addon"},
		{"with a port", "http://sample-addon:8080", "sample", "sample-addon"},
		{"fully qualified", "http://sample-addon.zaentrum.svc.cluster.local", "sample", "sample-addon"},
		{"upper case host", "http://Sample-Addon", "sample", "sample-addon"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := planAddon(c.address, decodeManifest(t, sampleManifest), "apps", "")
			if err != nil {
				t.Fatal(err)
			}
			want := model.AddonComponent{Name: c.service, Workload: c.workload, Role: "primary"}
			if len(plan.Components) != 1 || plan.Components[0] != want {
				t.Fatalf("components = %+v, want [%+v]", plan.Components, want)
			}
			if plan.Setup != nil {
				t.Errorf("no setup declared ⇒ none planned, got %+v", plan.Setup)
			}
		})
	}
}

// The implicit primary obeys the rules a declared one does: the address host
// is its workload and the service its name, so both must be DNS-1123 labels.
// Before this, http://[fe80::1] recorded workload "fe80::1", and
// http://localhost:8081 and :8082 collided on "localhost" with a misleading
// "already declared" conflict.
func TestPlanAddonRejectsAnAddressThatNamesNoService(t *testing.T) {
	cases := []struct {
		name, address, manifest, want string
	}{
		{"IPv6 literal", "http://[fe80::1]:8080", sampleManifest, "not an IP address"},
		{"IPv4 literal", "http://127.0.0.1:8080", sampleManifest, "not an IP address"},
		{"IPv4 literal with declared components", "http://127.0.0.1", `{"service":"example","components":[{"name":"example","workload":"127","role":"primary"}]}`, "not an IP address"},
		{"host that is no label", "http://under_score", sampleManifest, "DNS-1123 label"},
		{"service that is no label, no components", "http://example", `{"service":"Example_Addon"}`, "implicit primary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := planAddon(c.address, decodeManifest(t, c.manifest), "apps", "")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error mentioning %q, got %v", c.want, err)
			}
		})
	}
	// A service that is no label is only the implicit component's problem: a
	// manifest naming its components says what they are called.
	m := `{"service":"Example_Addon","components":[{"name":"example","workload":"example","role":"primary"}]}`
	if _, err := planAddon("http://example", decodeManifest(t, m), "apps", ""); err != nil {
		t.Errorf("declared components must not require a label service: %v", err)
	}
}

func TestSameAddress(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"http://example", "http://example", true},
		{"http://example", " HTTP://Example/ ", true},
		{"http://example", "http://example:80", true},
		{"https://example:443", "https://example", true},
		{"http://example:8080", "http://example:8080/", true},
		{"http://example", "http://example:8080", false},
		{"http://example", "https://example", false},
		{"http://example", "http://other", false},
		{"http://example.ns.svc", "http://example", false},
		{"http://example/base", "http://example", false},
	} {
		if got := sameAddress(c.a, c.b); got != c.same {
			t.Errorf("sameAddress(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}

func TestPlanAddonRejectsInvalidComponents(t *testing.T) {
	primary := `{"name":"example","workload":"example","role":"primary"}`
	many := make([]string, 0, 17)
	many = append(many, primary)
	for i := 0; i < 16; i++ {
		many = append(many, `{"name":"c`+string(rune('a'+i))+`","workload":"w`+string(rune('a'+i))+`","role":"optional"}`)
	}
	cases := []struct {
		name, components, want string
	}{
		{"too many", strings.Join(many, ","), "at most 16"},
		{"name not a label", primary + `,{"name":"Worker","workload":"w","role":"optional"}`, "name"},
		{"duplicate name", primary + `,{"name":"example","workload":"w","role":"optional"}`, "declared twice"},
		{"workload with dots", primary + `,{"name":"w","workload":"w.ns.svc","role":"optional"}`, "no dots"},
		{"empty workload", primary + `,{"name":"w","workload":"","role":"optional"}`, "workload"},
		{"duplicate workload", primary + `,{"name":"w","workload":"example","role":"optional"}`, "declared twice"},
		{"unknown role", primary + `,{"name":"w","workload":"w","role":"sidecar"}`, "role"},
		{"missing role", primary + `,{"name":"w","workload":"w"}`, "role"},
		{"summary too long", primary + `,{"name":"w","workload":"w","role":"optional","summary":"` + strings.Repeat("x", 121) + `"}`, "summary"},
		{"no primary", `{"name":"w","workload":"w","role":"required"}`, "exactly one primary"},
		{"two primaries", primary + `,{"name":"w","workload":"w","role":"primary"}`, "exactly one primary"},
		{"primary is not the install host", `{"name":"example","workload":"other","role":"primary"}`, "install address host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := `{"service":"example","components":[` + c.components + `]}`
			_, err := planAddon("http://example", decodeManifest(t, m), "apps", "")
			var me *manifestError
			if !errors.As(err, &me) {
				t.Fatalf("want a manifest error, got %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
	// Sixteen is allowed: the limit is a limit, not an off-by-one.
	m := `{"service":"example","components":[` + strings.Join(many[:16], ",") + `]}`
	if _, err := planAddon("http://example", decodeManifest(t, m), "apps", ""); err != nil {
		t.Errorf("16 components must install: %v", err)
	}
}

func TestPlanAddonRejectsInvalidSetup(t *testing.T) {
	section := func(extra string) string {
		return `{"key":"sources","title":"sources"` + extra + `}`
	}
	var seventeen []string
	for i := 0; i < 17; i++ {
		seventeen = append(seventeen, `{"key":"s`+string(rune('a'+i))+`"}`)
	}
	cases := []struct {
		name, setup, want string
	}{
		{"relative path", `{"path":"api/setup"}`, "start with '/'"},
		{"empty path", `{"path":""}`, "start with '/'"},
		{"double slash", `{"path":"//other/api/setup"}`, "'//'"},
		{"double slash inside", `{"path":"/api//setup"}`, "'//'"},
		{"dot dot", `{"path":"/api/../setup"}`, "'..'"},
		// Through the proxy with the admin's bearer, a browser resolves these
		// to /api/portal/debug/logs — out of the addon.
		{"encoded dot dot", `{"path":"/%2e%2e/%2e%2e/debug/logs"}`, "'..'"},
		{"half-encoded dot dot", `{"path":"/api/.%2e/setup"}`, "'..'"},
		{"upper-case encoded dot dot", `{"path":"/api/%2E./setup"}`, "'..'"},
		{"encoded slash", `{"path":"/api%2fsetup"}`, "encoded '/'"},
		{"encoded backslash", `{"path":"/api%5Csetup"}`, "encoded '/'"},
		{"invalid escape", `{"path":"/api/%zz"}`, "percent-escape"},
		{"target with encoded dots", `{"path":"/s","sections":[` + section(`,"target":"/%2e%2e/%2e%2e/settings"`) + `]}`, "'..'"},
		{"scheme", `{"path":"https://elsewhere/api/setup"}`, "start with '/'"},
		{"scheme after the slash", `{"path":"/redirect?to=https://elsewhere"}`, "'//'"},
		{"fragment", `{"path":"/api/setup#x"}`, "fragments"},
		{"too many sections", `{"path":"/s","sections":[` + strings.Join(seventeen, ",") + `]}`, "at most 16"},
		{"section key not a label", `{"path":"/s","sections":[{"key":"Sources"}]}`, "key"},
		{"duplicate section", `{"path":"/s","sections":[` + section("") + `,` + section("") + `]}`, "declared twice"},
		{"title too long", `{"path":"/s","sections":[` + section(`,"title":"`+strings.Repeat("t", 61)+`"`) + `]}`, "title"},
		{"target with scheme", `{"path":"/s","sections":[` + section(`,"target":"https://elsewhere/x"`) + `]}`, "inside the addon's console"},
		{"target javascript", `{"path":"/s","sections":[` + section(`,"target":"javascript:alert(1)"`) + `]}`, "inside the addon's console"},
		{"target protocol-relative", `{"path":"/s","sections":[` + section(`,"target":"//elsewhere/x"`) + `]}`, "inside the addon's console"},
		{"target climbing out", `{"path":"/s","sections":[` + section(`,"target":"../other"`) + `]}`, "'..'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := `{"service":"example","setup":` + c.setup + `}`
			_, err := planAddon("http://example", decodeManifest(t, m), "apps", "")
			var me *manifestError
			if !errors.As(err, &me) {
				t.Fatalf("want a manifest error, got %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// Tile targets follow the same rule as setup targets: somewhere inside the
// addon's console.
func TestValidTarget(t *testing.T) {
	for _, ok := range []string{"", "#/items", "?tab=queue", "/settings", "settings", "#/a/../b", "items?x=https"} {
		if err := validTarget(ok); err != nil {
			t.Errorf("validTarget(%q) = %v, want ok", ok, err)
		}
	}
	for _, ok := range []string{"/v%2e1/items", "#/%2e%2e/x", "items?back=%2e%2e"} {
		if err := validTarget(ok); err != nil {
			t.Errorf("validTarget(%q) = %v, want ok — an encoded dot that is no segment climbs nowhere", ok, err)
		}
	}
	for _, bad := range []string{
		"https://elsewhere", "//elsewhere", "javascript:alert(1)", "../up", "/a/../../b", "a\\b", "#/x\ny",
		// A browser resolves these exactly like "..".
		"/%2e%2e/%2e%2e/settings", "/a/.%2e/b", "%2E./x", "/%2E%2E",
		"/a%2fb", "/a%2Fb", "/a%5cb", "/bad%zz",
	} {
		if err := validTarget(bad); err == nil {
			t.Errorf("validTarget(%q) accepted, want rejected", bad)
		}
	}
	m := `{"service":"example","ui":{"tiles":[{"key":"x","title":"x","target":"https://elsewhere"}]}}`
	if _, err := planAddon("http://example", decodeManifest(t, m), "apps", ""); err == nil {
		t.Error("a tile target leaving the console must refuse the manifest")
	}
}

func TestInstallConflict(t *testing.T) {
	plan, err := planAddon("http://example", decodeManifest(t, groupManifest), "apps", "")
	if err != nil {
		t.Fatal(err)
	}
	installedApp := &model.App{Key: "example", BaseURL: "/portal/app/example", ProxyURL: "http://example"}
	installedAt := func(addr string) registered {
		return registered{app: installedApp, addon: &model.Addon{Key: "example", Address: addr}}
	}
	cases := []struct {
		name           string
		reg            registered
		replaceAddress bool
		platform       map[string]bool
		claims         map[string]string
		want           string // "" = no conflict
	}{
		{"fresh install", registered{}, false, nil, nil, ""},
		{"refresh of an installed addon", installedAt("http://example"), false, nil, map[string]string{"example": "example", "example-worker": "example"}, ""},
		{"refresh with the address spelled differently", installedAt("HTTP://Example:80/"), false, nil, nil, ""},
		{"key taken by a hand-registered app", registered{app: &model.App{Key: "example"}}, false, nil, nil, "not installed as an addon"},
		{"hand-registered app at the same address but another base", registered{app: &model.App{Key: "example", ProxyURL: "http://example", BaseURL: "/tools/example"}}, false, nil, nil, "not installed as an addon"},
		{"hand-registered app with the addon base at another address", registered{app: &model.App{Key: "example", ProxyURL: "http://other", BaseURL: "/portal/app/example"}}, false, nil, nil, "not installed as an addon"},
		// Installed before the addons table, with no tile and no slot row to
		// backfill it by: what the old install wrote, at the same address.
		{"tile-less addon from before the addons table is adopted", registered{app: installedApp}, false, nil, nil, ""},
		{"an installed addon moving to another address", installedAt("http://other"), false, nil, nil, "replaceAddress"},
		{"a confirmed move", installedAt("http://other"), true, nil, nil, ""},
		{"an addon recorded without an address has nothing to move", installedAt(""), false, nil, nil, ""},
		{"workload is a platform deployment", registered{}, false, map[string]bool{"example-worker": true}, nil, "platform deployment"},
		{"workload claimed by another addon", registered{}, false, nil, map[string]string{"example-worker": "other"}, `addon "other"`},
		{"unrelated claims do not conflict", registered{}, false, map[string]bool{"portal-api": true}, map[string]string{"other-worker": "other"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := installConflict(plan, c.reg, c.replaceAddress, c.platform, c.claims)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want no conflict, got %v", err)
				}
				return
			}
			var ce *conflictError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a conflict mentioning %q, got %v", c.want, err)
			}
		})
	}
}

func TestComponentViews(t *testing.T) {
	cs := []model.AddonComponent{
		{Name: "example", Workload: "example", Role: "primary"},
		{Name: "worker", Workload: "example-worker", Role: "optional"},
	}
	live := map[string]operator.Instance{
		"example": {Name: "example", Phase: "degraded", ReadyReplicas: 0, DesiredReplicas: 1, Restarts: 4, Reason: "CrashLoopBackOff"},
	}
	got := componentViews(cs, map[string][]string{"worker": {"example.item.done"}}, live, true)
	if got[0].Phase == nil || *got[0].Phase != "degraded" || *got[0].Ready != 0 || *got[0].Desired != 1 ||
		*got[0].Restarts != 4 || *got[0].Reason != "CrashLoopBackOff" {
		t.Errorf("deployed component = %+v", got[0])
	}
	// Not deployed: every state field null, so the console can say so.
	if got[1].Phase != nil || got[1].Ready != nil || got[1].Desired != nil || got[1].Restarts != nil || got[1].Reason != nil {
		t.Errorf("undeployed component must have null state: %+v", got[1])
	}
	if len(got[1].Topics) != 1 {
		t.Errorf("topics = %v", got[1].Topics)
	}
	b, _ := json.Marshal(got[1])
	if !strings.Contains(string(b), `"phase":null`) {
		t.Errorf("null state must serialise as null: %s", b)
	}
	// Cannot see workloads at all: unknown, which is not "not deployed".
	for _, v := range componentViews(cs, nil, nil, false) {
		if v.Phase == nil || *v.Phase != "unknown" || v.Ready != nil {
			t.Errorf("unavailable operator must report unknown: %+v", v)
		}
	}
}

func TestRemainingWorkloads(t *testing.T) {
	declared := []string{"example", "example-worker"}
	if got := remainingWorkloads(declared, nil); strings.Join(got, ",") != "example,example-worker" {
		t.Errorf("unknown live state must name every declared workload, got %v", got)
	}
	live := map[string]operator.Instance{"example-worker": {Name: "example-worker"}}
	if got := remainingWorkloads(declared, live); strings.Join(got, ",") != "example-worker" {
		t.Errorf("known live state names only what runs, got %v", got)
	}
	if got := remainingWorkloads(nil, live); got == nil || len(got) != 0 {
		t.Errorf("nothing declared must be an empty list, got %#v", got)
	}
}

// The hash must not depend on how the addon formatted its JSON — only on what
// it declared — or every addon would look refreshable forever.
func TestCanonicalManifestIgnoresFormatting(t *testing.T) {
	compact := decodeManifest(t, `{"service":"example","kind":"addon","version":"1"}`)
	spaced := decodeManifest(t, "{\n  \"version\": \"1\",\n  \"kind\": \"addon\",\n  \"service\": \"example\"\n}")
	_, a := canonicalManifest(compact)
	_, b := canonicalManifest(spaced)
	if a != b {
		t.Errorf("formatting changed the hash: %s != %s", a, b)
	}
	_, c := canonicalManifest(decodeManifest(t, `{"service":"example","kind":"addon","version":"2"}`))
	if a == c {
		t.Error("a different manifest must hash differently")
	}
}

// ─── handlers, against an in-memory registry ────────────────────────────────

type fakeAddonStore struct {
	apps     map[string]model.App
	addons   map[string]model.Addon
	claims   map[string]string
	installs []store.AddonInstall
	removals []string // key|declaredSpace
	removal  store.AddonRemoval
}

func newFakeStore() *fakeAddonStore {
	return &fakeAddonStore{apps: map[string]model.App{}, addons: map[string]model.Addon{}, claims: map[string]string{}}
}

func (f *fakeAddonStore) ListApps(context.Context) ([]model.App, error) {
	var out []model.App
	for _, a := range f.apps {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeAddonStore) GetApp(_ context.Context, key string) (*model.App, error) {
	if a, ok := f.apps[key]; ok {
		return &a, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAddonStore) ListSpaces(context.Context) ([]model.Space, error) {
	return []model.Space{{Key: "apps", Title: "apps"}}, nil
}

func (f *fakeAddonStore) ListAddons(context.Context) ([]model.Addon, error) {
	keys := make([]string, 0, len(f.addons))
	for key := range f.addons {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []model.Addon
	for _, key := range keys {
		out = append(out, f.addons[key])
	}
	return out, nil
}

func (f *fakeAddonStore) GetAddon(_ context.Context, key string) (*model.Addon, error) {
	if ad, ok := f.addons[key]; ok {
		return &ad, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAddonStore) WorkloadClaims(context.Context) (map[string]string, error) {
	return f.claims, nil
}

func (f *fakeAddonStore) InstallAddon(_ context.Context, in store.AddonInstall) error {
	f.installs = append(f.installs, in)
	return nil
}

func (f *fakeAddonStore) RemoveAddon(_ context.Context, key, declaredSpace string) (store.AddonRemoval, error) {
	f.removals = append(f.removals, key+"|"+declaredSpace)
	if _, ok := f.addons[key]; !ok {
		return store.AddonRemoval{}, store.ErrNotFound
	}
	return f.removal, nil
}

// manifestServer serves a descriptor on a host name — localhost — that is a
// valid workload label, so declared primaries can match the install address.
func manifestServer(t *testing.T, manifest string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wellKnownCapability {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(manifest))
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
}

// localhostManifest is groupManifest with its primary on localhost.
var localhostManifest = strings.Replace(groupManifest,
	`"workload": "example", "role": "primary"`, `"workload": "localhost", "role": "primary"`, 1)

func postInstall(t *testing.T, a *API, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	a.installAddon(rec, httptest.NewRequest(http.MethodPost, "/api/portal/addons", bytes.NewReader(b)))
	return rec
}

func TestInstallAddonDryRunWritesNothing(t *testing.T) {
	fake := newFakeStore()
	a := &API{addons: fake}
	addr := manifestServer(t, localhostManifest)

	rec := postInstall(t, a, map[string]any{"proxyUrl": addr, "dryRun": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body)
	}
	if len(fake.installs) != 0 {
		t.Fatalf("a dry run must write nothing, got %d installs", len(fake.installs))
	}
	var got struct {
		Key        string          `json:"key"`
		DryRun     bool            `json:"dryRun"`
		Refresh    bool            `json:"refresh"`
		Version    string          `json:"version"`
		Components []componentView `json:"components"`
		Setup      *ManifestSetup  `json:"setup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != "example" || !got.DryRun || got.Refresh || got.Version != "2.0.0" {
		t.Errorf("plan = %+v", got)
	}
	if len(got.Components) != 2 || got.Components[0].Workload != "localhost" {
		t.Errorf("components = %+v", got.Components)
	}
	// No operator here: the live state is unknown, not "not deployed".
	for _, c := range got.Components {
		if c.Phase == nil || *c.Phase != "unknown" {
			t.Errorf("component %q phase = %v, want unknown", c.Name, c.Phase)
		}
	}
	if got.Setup == nil || len(got.Setup.Sections) != 3 {
		t.Errorf("setup = %+v", got.Setup)
	}

	// The real install writes exactly one transaction's worth.
	rec = postInstall(t, a, map[string]any{"proxyUrl": addr})
	if rec.Code != http.StatusOK || len(fake.installs) != 1 {
		t.Fatalf("install = %d %s, installs=%d", rec.Code, rec.Body, len(fake.installs))
	}
	in := fake.installs[0]
	if in.App.Key != "example" || in.Addon.Key != "example" || in.Addon.Address != addr ||
		in.Addon.Version != "2.0.0" || len(in.Addon.ManifestSHA256) != 64 || len(in.Components) != 2 {
		t.Errorf("install = %+v", in)
	}
	var stored Descriptor
	if err := json.Unmarshal(in.Addon.Manifest, &stored); err != nil || stored.Setup == nil || stored.Setup.Path != "/api/setup" {
		t.Errorf("the stored manifest must round-trip the descriptor: %v %+v", err, stored)
	}
}

func TestInstallAddonRefusals(t *testing.T) {
	addr := manifestServer(t, localhostManifest)
	cases := []struct {
		name     string
		prepare  func(*fakeAddonStore)
		manifest string
		want     int
	}{
		{"hand-registered app with the same key", func(f *fakeAddonStore) {
			f.apps["example"] = model.App{Key: "example", Title: "example"}
		}, "", http.StatusConflict},
		{"workload claimed by another addon", func(f *fakeAddonStore) {
			f.claims["example-worker"] = "other"
		}, "", http.StatusConflict},
		{"refresh of an installed addon is not a conflict", func(f *fakeAddonStore) {
			f.apps["example"] = model.App{Key: "example", Title: "example"}
			f.addons["example"] = model.Addon{Key: "example"}
			f.claims["localhost"], f.claims["example-worker"] = "example", "example"
		}, "", http.StatusOK},
		{"invalid manifest", func(*fakeAddonStore) {},
			`{"service":"example","components":[{"name":"x","workload":"x","role":"required"}]}`, http.StatusUnprocessableEntity},
		{"tile-less addon installed before the addons table is adopted", func(f *fakeAddonStore) {
			f.apps["example"] = model.App{Key: "example", Title: "example", BaseURL: "/portal/app/example", ProxyURL: addr}
		}, "", http.StatusOK},
		{"moving an installed addon to another address", func(f *fakeAddonStore) {
			f.apps["example"] = model.App{Key: "example", BaseURL: "/portal/app/example", ProxyURL: "http://example"}
			f.addons["example"] = model.Addon{Key: "example", Address: "http://example"}
		}, "", http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeStore()
			c.prepare(fake)
			target := addr
			if c.manifest != "" {
				target = manifestServer(t, c.manifest)
			}
			rec := postInstall(t, &API{addons: fake}, map[string]any{"proxyUrl": target})
			if rec.Code != c.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
			}
			if c.want != http.StatusOK && len(fake.installs) != 0 {
				t.Fatal("a refused install must write nothing")
			}
		})
	}
}

// installResult is the part of an install answer these tests read.
type installResult struct {
	Refresh         bool            `json:"refresh"`
	Adopt           bool            `json:"adopt"`
	PreviousAddress string          `json:"previousAddress"`
	Components      []componentView `json:"components"`
}

func decodeInstall(t *testing.T, rec *httptest.ResponseRecorder) installResult {
	t.Helper()
	var got installResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return got
}

// A move to another address is shown by check and must be confirmed: any
// address serving a manifest with the same service would otherwise take over
// the addon's proxy, and the bearers it forwards.
func TestInstallAddonMoveNeedsConfirmation(t *testing.T) {
	addr := manifestServer(t, localhostManifest)
	fake := newFakeStore()
	fake.apps["example"] = model.App{Key: "example", BaseURL: "/portal/app/example", ProxyURL: "http://example"}
	fake.addons["example"] = model.Addon{Key: "example", Address: "http://example"}
	a := &API{addons: fake}

	rec := postInstall(t, a, map[string]any{"proxyUrl": addr, "dryRun": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("check of a move = %d %s — check shows it rather than refusing it", rec.Code, rec.Body)
	}
	if got := decodeInstall(t, rec); got.PreviousAddress != "http://example" || !got.Refresh {
		t.Errorf("check must name the address the addon moves from: %+v", got)
	}

	rec = postInstall(t, a, map[string]any{"proxyUrl": addr})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "replaceAddress") {
		t.Fatalf("unconfirmed move = %d %s, want 409 naming replaceAddress", rec.Code, rec.Body)
	}
	if len(fake.installs) != 0 {
		t.Fatal("an unconfirmed move must write nothing")
	}

	rec = postInstall(t, a, map[string]any{"proxyUrl": addr, "replaceAddress": true})
	if rec.Code != http.StatusOK || len(fake.installs) != 1 || fake.installs[0].Addon.Address != addr {
		t.Fatalf("confirmed move = %d %s, installs=%d", rec.Code, rec.Body, len(fake.installs))
	}

	// Refreshing from the recorded address needs no confirmation.
	fake.addons["example"] = model.Addon{Key: "example", Address: addr}
	if rec := postInstall(t, a, map[string]any{"proxyUrl": addr}); rec.Code != http.StatusOK {
		t.Errorf("refresh from the recorded address = %d %s", rec.Code, rec.Body)
	}
}

func TestInstallAddonAdoptionIsReported(t *testing.T) {
	addr := manifestServer(t, localhostManifest)
	fake := newFakeStore()
	fake.apps["example"] = model.App{Key: "example", BaseURL: "/portal/app/example", ProxyURL: addr + "/"}
	rec := postInstall(t, &API{addons: fake}, map[string]any{"proxyUrl": addr, "dryRun": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	if got := decodeInstall(t, rec); !got.Adopt || !got.Refresh || got.PreviousAddress != "" {
		t.Errorf("an adopted app must read as adopt and refresh: %+v", got)
	}
}

// fakeWorkloads is the operator service: in a cluster or not, listing or
// refusing.
type fakeWorkloads struct {
	available bool
	instances []operator.Instance
	err       error
}

func (f fakeWorkloads) Available() bool { return f.available }

func (f fakeWorkloads) Instances(context.Context) ([]operator.Instance, error) {
	return f.instances, f.err
}

func TestInstallAddonWorkloadVisibility(t *testing.T) {
	addr := manifestServer(t, localhostManifest)
	refused := fakeWorkloads{available: true, err: errors.New("apiserver: forbidden")}

	t.Run("in a cluster that refuses the list, install waits", func(t *testing.T) {
		fake := newFakeStore()
		a := &API{addons: fake, workloads: refused}
		rec := postInstall(t, a, map[string]any{"proxyUrl": addr})
		if rec.Code != http.StatusServiceUnavailable || len(fake.installs) != 0 {
			t.Fatalf("install = %d %s, installs=%d — want 503 and nothing written", rec.Code, rec.Body, len(fake.installs))
		}
		// A check still shows the plan, with the components unknown.
		rec = postInstall(t, a, map[string]any{"proxyUrl": addr, "dryRun": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("check = %d %s", rec.Code, rec.Body)
		}
		for _, c := range decodeInstall(t, rec).Components {
			if c.Phase == nil || *c.Phase != "unknown" {
				t.Errorf("component %q phase = %v, want unknown", c.Name, c.Phase)
			}
		}
	})

	t.Run("in a cluster that lists, a platform workload is refused", func(t *testing.T) {
		fake := newFakeStore()
		a := &API{addons: fake, workloads: fakeWorkloads{available: true, instances: []operator.Instance{
			{Name: "example-worker", Group: "platform", Phase: "ready"},
		}}}
		for _, dry := range []bool{true, false} {
			if rec := postInstall(t, a, map[string]any{"proxyUrl": addr, "dryRun": dry}); rec.Code != http.StatusConflict {
				t.Errorf("dryRun=%v = %d %s, want 409", dry, rec.Code, rec.Body)
			}
		}
	})

	t.Run("outside a cluster there are no workloads to take", func(t *testing.T) {
		fake := newFakeStore()
		a := &API{addons: fake, workloads: fakeWorkloads{available: false, err: errors.New("not in a cluster")}}
		if rec := postInstall(t, a, map[string]any{"proxyUrl": addr}); rec.Code != http.StatusOK || len(fake.installs) != 1 {
			t.Fatalf("install = %d %s", rec.Code, rec.Body)
		}
	})
}

func TestListAddons(t *testing.T) {
	d := decodeManifest(t, groupManifest)
	manifest, sum := canonicalManifest(d)
	fake := newFakeStore()
	fake.addons["example"] = model.Addon{
		Key: "example", Title: "Example", Address: "http://example", Version: "2.0.0",
		Manifest: manifest, ManifestSHA256: sum, Tiles: 1,
		Components: []model.AddonComponent{
			{Name: "example", Workload: "example", Role: "primary"},
			{Name: "worker", Workload: "example-worker", Role: "optional", Order: 1},
		},
	}
	// Installed before the addons table: no manifest, one implicit primary.
	fake.addons["legacy"] = model.Addon{
		Key: "legacy", Address: "http://legacy", Rows: 2,
		Components: []model.AddonComponent{{Name: "legacy", Workload: "legacy", Role: "primary"}},
	}
	a := &API{addons: fake}

	list := func(served ...Descriptor) []installedAddon {
		t.Helper()
		discCache.mu.Lock()
		discCache.fetched, discCache.doc, discCache.descs = time.Now(), []byte("{}"), served
		discCache.mu.Unlock()
		t.Cleanup(func() {
			discCache.mu.Lock()
			discCache.fetched, discCache.doc, discCache.descs = time.Time{}, nil, nil
			discCache.mu.Unlock()
		})
		rec := httptest.NewRecorder()
		a.listAddons(rec, httptest.NewRequest(http.MethodGet, "/api/portal/addons", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		var out []installedAddon
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := list(d)
	if len(out) != 2 {
		t.Fatalf("addons = %+v", out)
	}
	ex, legacy := out[0], out[1]
	if ex.Key != "example" || ex.Version != "2.0.0" || ex.ProxyURL != "http://example" || len(ex.Components) != 2 {
		t.Errorf("example = %+v", ex)
	}
	if ex.Setup == nil || ex.Setup.Path != "/api/setup" || ex.Setup.Sections[0].Key != "sources" {
		t.Errorf("setup must come from the stored manifest, in order: %+v", ex.Setup)
	}
	if len(ex.Components[1].Topics) != 1 {
		t.Errorf("topics must come from the stored manifest: %+v", ex.Components[1])
	}
	if ex.RefreshAvailable {
		t.Error("the addon serves what was installed — nothing to refresh")
	}
	if legacy.Title != "legacy" || legacy.Setup != nil || legacy.Slots != 2 || len(legacy.Components) != 1 {
		t.Errorf("backfilled addon = %+v", legacy)
	}
	if legacy.RefreshAvailable {
		t.Error("an addon the discovery cache does not hold cannot be called refreshable")
	}

	d.Version = "2.1.0"
	if out := list(d); !out[0].RefreshAvailable {
		t.Error("a redeployed addon serving a different manifest must offer a refresh")
	}
}

func TestRemoveAddon(t *testing.T) {
	d := decodeManifest(t, `{"service":"example","ui":{"space":{"key":"example-space"}}}`)
	manifest, _ := canonicalManifest(d)
	fake := newFakeStore()
	fake.addons["example"] = model.Addon{Key: "example", Manifest: manifest}
	fake.removal = store.AddonRemoval{Tiles: 3, Rows: 1, Spaces: []string{"example-space"}, Workloads: []string{"example", "example-worker"}}
	a := &API{addons: fake}

	del := func(key string) *httptest.ResponseRecorder {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("key", key)
		r := httptest.NewRequest(http.MethodDelete, "/api/portal/addons/"+key, nil)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		a.removeAddon(rec, r)
		return rec
	}

	rec := del("example")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	if len(fake.removals) != 1 || fake.removals[0] != "example|example-space" {
		t.Errorf("the declared space must be the one offered for removal: %v", fake.removals)
	}
	var got struct {
		Removed struct {
			Tiles int    `json:"tiles"`
			Rows  int    `json:"rows"`
			Space string `json:"space"`
		} `json:"removed"`
		RemainingWorkloads []string `json:"remainingWorkloads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Removed.Tiles != 3 || got.Removed.Rows != 1 || got.Removed.Space != "example-space" {
		t.Errorf("removed = %+v", got.Removed)
	}
	// Without an operator the platform cannot tell what still runs, so it
	// names every declared workload.
	if strings.Join(got.RemainingWorkloads, ",") != "example,example-worker" {
		t.Errorf("remainingWorkloads = %v", got.RemainingWorkloads)
	}

	if rec := del("missing"); rec.Code != http.StatusNotFound {
		t.Errorf("removing an unknown addon = %d, want 404", rec.Code)
	}
}
