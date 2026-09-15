package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

const addonPlural = "zaentrumaddons"

// chartEnv is portal-api's router against a fake apiserver and an in-memory
// registry, signed in as an admin.
type chartEnv struct {
	t     *testing.T
	api   *API
	kube  *k8sfake.Server
	store *fakeAddonStore
	h     http.Handler
}

func newChartEnv(t *testing.T) *chartEnv {
	t.Helper()
	kube := k8sfake.New(t)
	cfg := config.Config{OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", AddonPlural: addonPlural, AdminRole: "zaentrum-admin"}
	st := newFakeStore()
	a := &API{addons: st, cfg: cfg, charts: operator.New(kube.Client("zaentrum"), cfg)}
	a.registration.kick = make(chan struct{}, 1)
	jwt, err := auth.NewJWTVerifier(context.Background(), "", "", cfg.AdminRole, false, true)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	a.Register(r, auth.NewMiddleware(jwt, cfg.AdminRole, "zaentrum-addon"))
	return &chartEnv{t: t, api: a, kube: kube, store: st, h: r}
}

func (e *chartEnv) do(method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr *bytes.Reader
	if s, ok := body.(string); ok {
		rdr = bytes.NewReader([]byte(s))
	} else if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest(method, target, rdr))
	return rec
}

func (e *chartEnv) spec(name string) map[string]any {
	e.t.Helper()
	obj := e.kube.Object(addonPlural, name)
	if obj == nil {
		e.t.Fatalf("no ZaentrumAddon %q", name)
	}
	spec, _ := obj["spec"].(map[string]any)
	return spec
}

// seed stores a ZaentrumAddon the way the operator and an earlier request
// left it.
func (e *chartEnv) seed(name string, spec, status map[string]any) {
	e.kube.Put(addonPlural, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "ZaentrumAddon",
		"metadata": map[string]any{"name": name},
		"spec":     spec,
	})
	if status != nil {
		e.kube.SetStatus(addonPlural, name, status)
	}
}

// plans reports a plan for the addon's current generation, as the operator does.
func (e *chartEnv) plans(name, phase string, plan map[string]any, extra map[string]any) {
	status := map[string]any{"phase": phase, "observedGeneration": e.kube.Generation(addonPlural, name), "plan": plan}
	for k, v := range extra {
		status[k] = v
	}
	e.kube.SetStatus(addonPlural, name, status)
}

// examplePlan is a clean plan: one primary, one worker.
func examplePlan(version string) map[string]any {
	return map[string]any{
		"chart": map[string]any{
			"name": "example", "version": version, "appVersion": "2.0.0", "description": "an example addon",
			"annotations": map[string]any{"zaentrum.io/addon": "true", "zaentrum.io/primary": "example"},
		},
		"valuesSchema": `{"type":"object","properties":{"config":{"type":"object","properties":{"password":{"type":"string","writeOnly":true}}}}}`,
		"valuesErrors": []any{},
		"violations":   []any{},
		"objects":      []any{map[string]any{"kind": "Deployment", "name": "example"}, map[string]any{"kind": "Deployment", "name": "example-worker"}},
		"workloads": []any{
			map[string]any{"kind": "Deployment", "name": "example", "images": []any{"ghcr.io/example/example:" + version}, "ports": []any{8080}},
			map[string]any{"kind": "Deployment", "name": "example-worker", "images": []any{"ghcr.io/example/example-worker:" + version}, "ports": []any{}},
		},
	}
}

func secretData(t *testing.T, kube *k8sfake.Server, name string) map[string]string {
	t.Helper()
	sec := kube.Secret(name)
	if sec == nil {
		return nil
	}
	out := map[string]string{}
	data, _ := sec["data"].(map[string]any)
	for k, v := range data {
		b, err := base64.StdEncoding.DecodeString(v.(string))
		if err != nil {
			t.Fatalf("secret %s key %s: %v", name, k, err)
		}
		out[k] = string(b)
	}
	return out
}

// secretCalls is every request portal-api made against Secrets, as "METHOD name".
func secretCalls(kube *k8sfake.Server) []string {
	var out []string
	for _, c := range kube.Calls() {
		if i := strings.Index(c.Path, "/secrets"); i >= 0 {
			out = append(out, c.Method+" "+strings.TrimPrefix(c.Path[i:], "/secrets"))
		}
	}
	return out
}

func TestNormaliseChart(t *testing.T) {
	ok := []struct {
		ref, version, digest string
		want                 operator.ChartSource
		name                 string
	}{
		{"oci://registry.example.org/charts/example", "1.2.0", "", operator.ChartSource{Ref: "oci://registry.example.org/charts/example", Version: "1.2.0"}, "example"},
		{"oci://registry.example.org/charts/example:1.2.0", "", "", operator.ChartSource{Ref: "oci://registry.example.org/charts/example", Version: "1.2.0"}, "example"},
		{" OCI://registry.example.org:5000/example:1.2.0+build.1 ", "1.2.0+build.1", "", operator.ChartSource{Ref: "oci://registry.example.org:5000/example", Version: "1.2.0+build.1"}, "example"},
		{"https://charts.example.org/example-1.2.0.tgz", "9.9.9", "sha256:" + strings.Repeat("a", 64),
			operator.ChartSource{Ref: "https://charts.example.org/example-1.2.0.tgz", Digest: "sha256:" + strings.Repeat("a", 64)}, "example"},
		{"https://charts.example.org/dl/example-worker-v0.3.1-rc.1.tar.gz?token=x", "", "", operator.ChartSource{Ref: "https://charts.example.org/dl/example-worker-v0.3.1-rc.1.tar.gz?token=x"}, "example-worker"},
	}
	for _, c := range ok {
		got, err := normaliseChart(c.ref, c.version, c.digest)
		if err != nil || got != c.want {
			t.Errorf("normaliseChart(%q, %q) = %+v, %v; want %+v", c.ref, c.version, got, err, c.want)
		}
		if name := defaultAddonName(got.Ref); name != c.name {
			t.Errorf("defaultAddonName(%q) = %q, want %q", got.Ref, name, c.name)
		}
	}
	refused := []struct{ ref, version, digest, want string }{
		{"", "", "", "chart is required"},
		{"http://charts.example.org/example-1.2.0.tgz", "", "", "oci:// reference or an https:// link"},
		{"file:///tmp/example.tgz", "", "", "oci:// reference or an https:// link"},
		{"oci://registry.example.org/charts/example", "", "", "version is required"},
		{"oci://registry.example.org/charts/example:1.2.0", "1.3.0", "", "does not match the tag"},
		{"oci://registry.example.org/charts/example@sha256:" + strings.Repeat("a", 64), "", "", "pin an oci:// chart with digest"},
		{"oci://registry.example.org", "1.0.0", "", "a registry and a repository"},
		{"oci://registry.example.org/charts/example", "1.0.0 beta", "", "not a valid chart version"},
		{"https://user:pass@charts.example.org/example.tgz", "", "", "credentials"},
		{"https://charts.example.org/example 1.tgz", "", "", "spaces"},
		{"oci://registry.example.org/charts/example", "1.0.0", "sha256:ABC", "64 lower-case hex"},
	}
	for _, c := range refused {
		_, err := normaliseChart(c.ref, c.version, c.digest)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("normaliseChart(%q, %q, %q) = %v, want an error mentioning %q", c.ref, c.version, c.digest, err, c.want)
		}
	}
}

func TestCreateAddonChart(t *testing.T) {
	e := newChartEnv(t)
	rec := e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{
		"chart":        "oci://registry.example.org/charts/example:1.2.0",
		"values":       map[string]any{"worker": map[string]any{"replicas": 2}},
		"secretValues": map[string]string{"config.password": "s3cret-value"},
	})
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != `{"name":"example"}` {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	spec := e.spec("example")
	chart := spec["chart"].(map[string]any)
	if chart["ref"] != "oci://registry.example.org/charts/example" || chart["version"] != "1.2.0" || spec["suspend"] != true {
		t.Errorf("spec = %v — a new addon is planned, not applied", spec)
	}
	if b, _ := json.Marshal(spec["values"]); string(b) != `{"worker":{"replicas":2}}` {
		t.Errorf("values = %s", b)
	}
	wantFrom := `[{"kind":"Secret","name":"zaentrum-addon-example-values","targetPath":"config.password","valuesKey":"config.password"}]`
	if b, _ := json.Marshal(spec["valuesFrom"]); string(b) != wantFrom {
		t.Errorf("valuesFrom = %s", b)
	}
	if got := secretData(t, e.kube, "zaentrum-addon-example-values"); got["config.password"] != "s3cret-value" || len(got) != 1 {
		t.Errorf("values secret = %v", got)
	}
	labels := e.kube.Secret("zaentrum-addon-example-values")["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["zaentrum.io/addon"] != "example" {
		t.Errorf("secret labels = %v", labels)
	}

	// Adding it again updates it: values replaced, secret inputs added to the
	// ones already set, planned again.
	e.plans("example", "Planned", examplePlan("1.2.0"), nil)
	rec = e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{
		"name": "example", "chart": "https://charts.example.org/example-1.3.0.tgz",
		"values":       map[string]any{"logLevel": "debug"},
		"secretValues": map[string]string{"database.url": "postgres://db.example.org/example"},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}
	spec = e.spec("example")
	if b, _ := json.Marshal(spec["values"]); string(b) != `{"logLevel":"debug"}` {
		t.Errorf("values after update = %s", b)
	}
	if chart := spec["chart"].(map[string]any); chart["ref"] != "https://charts.example.org/example-1.3.0.tgz" || chart["version"] != nil {
		t.Errorf("chart after update = %v — an archive link carries no version", chart)
	}
	var from []operator.ValuesFrom
	b, _ := json.Marshal(spec["valuesFrom"])
	_ = json.Unmarshal(b, &from)
	if len(from) != 2 || from[0].TargetPath != "config.password" || from[1].TargetPath != "database.url" {
		t.Errorf("valuesFrom after update = %s", b)
	}
	if got := secretData(t, e.kube, "zaentrum-addon-example-values"); len(got) != 2 {
		t.Errorf("values secret after update = %v", got)
	}
	for _, c := range secretCalls(e.kube) {
		if strings.HasPrefix(c, "GET") {
			t.Errorf("secret read: %s", c)
		}
	}
}

func TestCreateAddonChartRefusals(t *testing.T) {
	chart := "oci://registry.example.org/charts/example:1.2.0"
	cases := []struct {
		name    string
		prepare func(*chartEnv)
		body    any
		want    int
		mention string
	}{
		{"no chart", nil, map[string]any{}, http.StatusBadRequest, "chart is required"},
		{"not json", nil, "{", http.StatusBadRequest, "invalid json"},
		{"derived name is no label", nil, map[string]any{"chart": "https://charts.example.org/Example_Chart-1.0.0.tgz"}, http.StatusBadRequest, "DNS-1123"},
		{"name too long", nil, map[string]any{"chart": chart, "name": strings.Repeat("a", 41)}, http.StatusBadRequest, "at most 40"},
		{"values not an object", nil, map[string]any{"chart": chart, "values": []int{1}}, http.StatusBadRequest, "JSON object"},
		{"values set the platform's key", nil, map[string]any{"chart": chart, "values": map[string]any{"zaentrum": map[string]any{"hostname": "x"}}}, http.StatusBadRequest, "reserved"},
		{"secret path with an empty segment", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"config..password": "x"}}, http.StatusBadRequest, "dotted values path"},
		{"secret path under the platform's key", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"zaentrum.issuer": "x"}}, http.StatusBadRequest, "reserved"},
		{"empty secret", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"config.password": ""}}, http.StatusBadRequest, "clearSecrets"},
		{"name of an addon added by address", func(e *chartEnv) {
			e.store.addons["example"] = model.Addon{Key: "example", Address: "http://example"}
		}, map[string]any{"chart": chart}, http.StatusConflict, "http://example"},
		{"key of an app registered by hand", func(e *chartEnv) {
			e.store.apps["example"] = model.App{Key: "example", Title: "example"}
		}, map[string]any{"chart": chart}, http.StatusConflict, "already registered"},
		{"cluster without the resource type", func(e *chartEnv) {
			e.kube.Unserved[addonPlural] = true
		}, map[string]any{"chart": chart, "secretValues": map[string]string{"config.password": "x"}}, http.StatusServiceUnavailable, "update the zaentrum-operator"},
		{"role without the resource", func(e *chartEnv) {
			e.kube.Forbidden[addonPlural] = true
		}, map[string]any{"chart": chart}, http.StatusServiceUnavailable, "Role"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newChartEnv(t)
			if c.prepare != nil {
				c.prepare(e)
			}
			rec := e.do(http.MethodPost, "/api/portal/addon-charts", c.body)
			if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.mention) {
				t.Fatalf("status = %d %q, want %d mentioning %q", rec.Code, strings.TrimSpace(rec.Body.String()), c.want, c.mention)
			}
			if e.kube.Object(addonPlural, "example") != nil || e.kube.Secret("zaentrum-addon-example-values") != nil {
				t.Error("a refused request must write nothing")
			}
		})
	}

	t.Run("outside a cluster", func(t *testing.T) {
		a := &API{addons: newFakeStore()}
		for _, h := range []http.HandlerFunc{a.createAddonChart, a.getAddonChart, a.patchAddonChart, a.installAddonChart, a.removeAddonChart} {
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodPost, "/api/portal/addon-charts", strings.NewReader(`{}`)))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		a.addonChartsStatus(rec, httptest.NewRequest(http.MethodGet, "/api/portal/addon-charts", nil))
		if !strings.Contains(rec.Body.String(), `"available":false`) {
			t.Errorf("status = %s", rec.Body)
		}
	})
}

func TestAddonChartsStatus(t *testing.T) {
	e := newChartEnv(t)
	if rec := e.do(http.MethodGet, "/api/portal/addon-charts", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":true`) {
		t.Errorf("available = %d %s", rec.Code, rec.Body)
	}
	e.kube.Unserved[addonPlural] = true
	rec := e.do(http.MethodGet, "/api/portal/addon-charts", nil)
	if !strings.Contains(rec.Body.String(), `"available":false`) || !strings.Contains(rec.Body.String(), "ZaentrumAddon") {
		t.Errorf("unserved = %s", rec.Body)
	}
}

func TestGetAddonChart(t *testing.T) {
	e := newChartEnv(t)
	e.seed("example", map[string]any{
		"chart":   map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"},
		"values":  map[string]any{"worker": map[string]any{"replicas": 2}},
		"suspend": true,
		"valuesFrom": []any{
			map[string]any{"kind": "ConfigMap", "name": "shared-values", "valuesKey": "values.yaml"},
			map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values", "valuesKey": "config.password", "targetPath": "config.password"},
			map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-generated", "valuesKey": "config.key", "targetPath": "config.key"},
		},
	}, nil)
	plan := examplePlan("1.2.0")
	plan["valuesErrors"] = []any{"config.password: required"}
	e.plans("example", "PlanFailed", plan, map[string]any{"message": "values do not validate"})
	e.kube.PutSecret(map[string]any{"metadata": map[string]any{"name": "zaentrum-addon-example-values"}, "data": map[string]any{"config.password": "czNjcmV0LXZhbHVl"}})

	rec := e.do(http.MethodGet, "/api/portal/addon-charts/example", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "czNjcmV0LXZhbHVl") || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatal("a secret value must never appear in an answer")
	}
	var got struct {
		Name                           string                `json:"name"`
		Suspended                      bool                  `json:"suspended"`
		Phase                          string                `json:"phase"`
		Message                        string                `json:"message"`
		Plan                           map[string]any        `json:"plan"`
		Components                     []any                 `json:"components"`
		LastApplied                    *operator.ChartSource `json:"lastAppliedChart"`
		Values                         map[string]any        `json:"values"`
		SecretKeys                     []string              `json:"secretKeys"`
		Registered                     bool                  `json:"registered"`
		Generation, ObservedGeneration int64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "example" || !got.Suspended || got.Phase != "PlanFailed" || got.Message != "values do not validate" || got.Registered {
		t.Errorf("view = %+v", got)
	}
	if got.Plan["valuesSchema"] == "" || len(got.Plan["workloads"].([]any)) != 2 || len(got.Plan["valuesErrors"].([]any)) != 1 {
		t.Errorf("the plan must pass through whole: %v", got.Plan)
	}
	if got.Components == nil || got.LastApplied != nil || got.Values["worker"] == nil {
		t.Errorf("components/lastApplied/values = %v %v %v", got.Components, got.LastApplied, got.Values)
	}
	// Only the inputs portal-api manages: not a shared values document, not
	// the operator's generated Secret.
	if strings.Join(got.SecretKeys, ",") != "config.password" {
		t.Errorf("secretKeys = %v", got.SecretKeys)
	}

	e.store.addons["example"] = model.Addon{Key: "example", ChartRef: "oci://registry.example.org/charts/example", ChartVersion: "1.2.0"}
	if rec := e.do(http.MethodGet, "/api/portal/addon-charts/example", nil); !strings.Contains(rec.Body.String(), `"registered":true`) {
		t.Errorf("registered = %s", rec.Body)
	}
	if rec := e.do(http.MethodGet, "/api/portal/addon-charts/other", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(http.MethodGet, "/api/portal/addon-charts/Not_A_Name", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid name = %d", rec.Code)
	}
	e.kube.Unserved[addonPlural] = true
	if rec := e.do(http.MethodGet, "/api/portal/addon-charts/example", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unserved = %d %s", rec.Code, rec.Body)
	}
	if calls := secretCalls(e.kube); len(calls) != 0 {
		t.Errorf("reading an addon must not touch its Secret: %v", calls)
	}
}

func TestInstallAddonChart(t *testing.T) {
	spec := map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}, "suspend": true}
	install := func(e *chartEnv) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, "/api/portal/addon-charts/example/install", nil)
	}
	suspended := func(e *chartEnv) bool { return e.spec("example")["suspend"] == true }

	t.Run("no plan yet", func(t *testing.T) {
		e := newChartEnv(t)
		e.seed("example", spec, map[string]any{"phase": "Pending"})
		if rec := install(e); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no plan") || !suspended(e) {
			t.Fatalf("install = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a plan for an earlier generation", func(t *testing.T) {
		e := newChartEnv(t)
		e.seed("example", spec, nil)
		e.plans("example", "Planned", examplePlan("1.2.0"), nil)
		// The admin changed the addon since: the plan describes the old spec.
		if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", map[string]any{"values": map[string]any{"a": 1}}); rec.Code != http.StatusAccepted {
			t.Fatalf("patch = %d %s", rec.Code, rec.Body)
		}
		if rec := install(e); rec.Code != http.StatusConflict || !suspended(e) {
			t.Fatalf("install = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("violations", func(t *testing.T) {
		e := newChartEnv(t)
		e.seed("example", spec, nil)
		plan := examplePlan("1.2.0")
		plan["violations"] = []any{"Deployment/example-worker: hostPath volume not allowed"}
		e.plans("example", "PlanFailed", plan, nil)
		if rec := install(e); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "hostPath") || !suspended(e) {
			t.Fatalf("install = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("values errors", func(t *testing.T) {
		e := newChartEnv(t)
		e.seed("example", spec, nil)
		plan := examplePlan("1.2.0")
		plan["valuesErrors"] = []any{"config.password: required"}
		e.plans("example", "PlanFailed", plan, nil)
		if rec := install(e); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "config.password") || !suspended(e) {
			t.Fatalf("install = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a clean plan installs and wakes registration", func(t *testing.T) {
		e := newChartEnv(t)
		e.seed("example", spec, nil)
		e.plans("example", "Planned", examplePlan("1.2.0"), nil)
		if rec := install(e); rec.Code != http.StatusAccepted || suspended(e) {
			t.Fatalf("install = %d %s, suspend=%v", rec.Code, rec.Body, e.spec("example")["suspend"])
		}
		select {
		case <-e.api.registration.kick:
		default:
			t.Error("install must kick the registration loop")
		}
		// Installing what installs already writes nothing.
		gen := e.kube.Generation(addonPlural, "example")
		puts := 0
		e.plans("example", "Installing", examplePlan("1.2.0"), nil)
		if rec := install(e); rec.Code != http.StatusAccepted {
			t.Fatalf("second install = %d %s", rec.Code, rec.Body)
		}
		for _, c := range e.kube.Calls() {
			if c.Method == http.MethodPut {
				puts++
			}
		}
		if puts != 1 || e.kube.Generation(addonPlural, "example") != gen {
			t.Errorf("puts = %d, generation %d → %d", puts, gen, e.kube.Generation(addonPlural, "example"))
		}
	})
	t.Run("missing", func(t *testing.T) {
		e := newChartEnv(t)
		if rec := install(e); rec.Code != http.StatusNotFound {
			t.Fatalf("install = %d %s", rec.Code, rec.Body)
		}
	})
}

func TestPatchAddonChart(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	installed := func(e *chartEnv) {
		e.seed("example", map[string]any{
			"chart":   map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0", "digest": digest},
			"values":  map[string]any{"worker": map[string]any{"replicas": 2}},
			"suspend": false,
			"valuesFrom": []any{
				map[string]any{"kind": "ConfigMap", "name": "shared-values"},
				map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values", "valuesKey": "config.password", "targetPath": "config.password"},
			},
		}, map[string]any{"phase": "Ready", "lastAppliedChart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}})
		e.kube.PutSecret(map[string]any{
			"metadata": map[string]any{"name": "zaentrum-addon-example-values", "labels": map[string]any{"zaentrum.io/addon": "example"}},
			"data":     map[string]any{"config.password": base64.StdEncoding.EncodeToString([]byte("old"))},
		})
	}
	patch := func(e *chartEnv, body any) {
		t.Helper()
		if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", body); rec.Code != http.StatusAccepted {
			t.Fatalf("patch %v = %d %s", body, rec.Code, rec.Body)
		}
	}

	t.Run("upgrade plans first", func(t *testing.T) {
		e := newChartEnv(t)
		installed(e)
		patch(e, map[string]any{"version": "1.3.0"})
		spec := e.spec("example")
		chart := spec["chart"].(map[string]any)
		if chart["version"] != "1.3.0" || chart["digest"] != nil || spec["suspend"] != true {
			t.Errorf("spec = %v — a new version drops the old digest and is planned first", spec)
		}
		if spec["values"] == nil || len(spec["valuesFrom"].([]any)) != 2 {
			t.Errorf("an upgrade keeps values and inputs: %v", spec)
		}
		// Applying the upgrade straight away is the admin's choice.
		patch(e, map[string]any{"version": "1.4.0", "digest": digest, "suspend": false})
		spec = e.spec("example")
		if chart := spec["chart"].(map[string]any); chart["version"] != "1.4.0" || chart["digest"] != digest || spec["suspend"] != false {
			t.Errorf("spec = %v", spec)
		}
		// A reference with a tag says which version.
		patch(e, map[string]any{"chart": "oci://registry.example.org/charts/example:2.0.0"})
		if chart := e.spec("example")["chart"].(map[string]any); chart["version"] != "2.0.0" || chart["ref"] != "oci://registry.example.org/charts/example" {
			t.Errorf("chart = %v", chart)
		}
	})

	t.Run("reconfigure applies in place", func(t *testing.T) {
		e := newChartEnv(t)
		installed(e)
		patch(e, map[string]any{
			"values":       map[string]any{"logLevel": "debug"},
			"secretValues": map[string]string{"database.url": "postgres://db.example.org/example"},
			"clearSecrets": []string{"config.password"},
		})
		spec := e.spec("example")
		if spec["suspend"] != false {
			t.Errorf("values alone must not suspend an installed addon: %v", spec)
		}
		if b, _ := json.Marshal(spec["values"]); string(b) != `{"logLevel":"debug"}` {
			t.Errorf("values = %s", b)
		}
		if b, _ := json.Marshal(spec["valuesFrom"]); string(b) != `[{"kind":"ConfigMap","name":"shared-values"},{"kind":"Secret","name":"zaentrum-addon-example-values","targetPath":"database.url","valuesKey":"database.url"}]` {
			t.Errorf("valuesFrom = %s — other entries stay first, in place", b)
		}
		if got := secretData(t, e.kube, "zaentrum-addon-example-values"); len(got) != 1 || got["database.url"] == "" {
			t.Errorf("secret = %v", got)
		}
		// Written before the resource references it; cleared after it no
		// longer does.
		var order []string
		for _, c := range e.kube.Calls() {
			switch {
			case c.Method == http.MethodPatch && strings.Contains(c.Path, "/secrets/"):
				if strings.Contains(c.Body, `"config.password":null`) {
					order = append(order, "clear")
				} else {
					order = append(order, "set")
				}
			case c.Method == http.MethodPut:
				order = append(order, "resource")
			}
		}
		if strings.Join(order, ",") != "set,resource,clear" {
			t.Errorf("order = %v", order)
		}
		patch(e, map[string]any{"values": nil})
		if _, ok := e.spec("example")["values"]; ok {
			t.Error("values: null removes them")
		}
	})

	t.Run("refusals", func(t *testing.T) {
		e := newChartEnv(t)
		installed(e)
		for _, c := range []struct {
			body    any
			want    int
			mention string
		}{
			{map[string]any{"secretValues": map[string]string{"a": "x"}, "clearSecrets": []string{"a"}}, http.StatusBadRequest, "both set and cleared"},
			{map[string]any{"version": ""}, http.StatusBadRequest, "version is required"},
			{map[string]any{"chart": "http://charts.example.org/example.tgz"}, http.StatusBadRequest, "https://"},
			{map[string]any{"clearSecrets": []string{"../x"}}, http.StatusBadRequest, "dotted values path"},
		} {
			rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", c.body)
			if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.mention) {
				t.Errorf("patch %v = %d %s, want %d mentioning %q", c.body, rec.Code, rec.Body, c.want, c.mention)
			}
		}
		if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/other", map[string]any{"secretValues": map[string]string{"a": "x"}}); rec.Code != http.StatusNotFound {
			t.Errorf("patch of a missing addon = %d %s", rec.Code, rec.Body)
		}
		if e.kube.Secret("zaentrum-addon-other-values") != nil {
			t.Error("a patch of a missing addon must not write its Secret")
		}
		if e.kube.Generation(addonPlural, "example") != 1 {
			t.Error("refused patches must not change the addon")
		}
	})
}

func TestRemoveAddonChart(t *testing.T) {
	prepare := func(e *chartEnv, registered bool) {
		e.seed("example", map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}}, nil)
		e.kube.PutSecret(map[string]any{
			"metadata": map[string]any{
				"name":            "zaentrum-addon-example-values",
				"labels":          map[string]any{"zaentrum.io/addon": "example"},
				"ownerReferences": []any{map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "ZaentrumAddon", "name": "example"}},
			},
			"data": map[string]any{"config.password": base64.StdEncoding.EncodeToString([]byte("s3cret"))},
		})
		if registered {
			e.store.addons["example"] = model.Addon{Key: "example", ChartRef: "oci://registry.example.org/charts/example", ChartVersion: "1.2.0"}
		}
	}

	t.Run("keep values", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, true)
		rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=true", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"keptValues":true`) {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body)
		}
		if e.kube.Object(addonPlural, "example") != nil {
			t.Error("the ZaentrumAddon must be deleted")
		}
		sec := e.kube.Secret("zaentrum-addon-example-values")
		if sec == nil {
			t.Fatal("a kept values Secret must survive its addon's garbage collection")
		}
		md := sec["metadata"].(map[string]any)
		if md["labels"].(map[string]any)["zaentrum.io/keep"] != "true" || md["ownerReferences"] != nil {
			t.Errorf("kept secret = %v", md)
		}
		if len(e.store.removals) != 1 || e.store.removals[0] != "example|" {
			t.Errorf("registry rows must go: %v", e.store.removals)
		}
		// The keep label lands before the resource is deleted.
		calls := e.kube.Calls()
		keptAt, deletedAt := -1, -1
		for i, c := range calls {
			if c.Method == http.MethodPatch && strings.Contains(c.Path, "/secrets/") {
				keptAt = i
			}
			if c.Method == http.MethodDelete && strings.Contains(c.Path, addonPlural) {
				deletedAt = i
			}
		}
		if keptAt < 0 || deletedAt < 0 || keptAt > deletedAt {
			t.Errorf("keep at %d, delete at %d", keptAt, deletedAt)
		}
	})

	t.Run("without keeping values", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, true)
		// Not owned yet — the operator never reconciled — and still deleted.
		e.kube.PutSecret(map[string]any{"metadata": map[string]any{"name": "zaentrum-addon-example-values"}, "data": map[string]any{}})
		rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example", nil)
		if rec.Code != http.StatusOK || e.kube.Secret("zaentrum-addon-example-values") != nil || e.kube.Object(addonPlural, "example") != nil {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body)
		}
		if len(e.store.removals) != 1 {
			t.Errorf("removals = %v", e.store.removals)
		}
	})

	t.Run("cancel a plan", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, false)
		rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resource":true`) || len(e.store.removals) != 0 {
			t.Fatalf("cancel = %d %s, removals %v", rec.Code, rec.Body, e.store.removals)
		}
	})

	t.Run("an addon of that key added by address stays", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, false)
		e.store.addons["example"] = model.Addon{Key: "example", Address: "http://example"}
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example", nil); rec.Code != http.StatusOK || len(e.store.removals) != 0 {
			t.Fatalf("remove = %d %s, removals %v", rec.Code, rec.Body, e.store.removals)
		}
	})

	t.Run("rows of a resource deleted elsewhere", func(t *testing.T) {
		e := newChartEnv(t)
		e.store.addons["example"] = model.Addon{Key: "example", ChartRef: "oci://registry.example.org/charts/example"}
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example", nil); rec.Code != http.StatusOK || len(e.store.removals) != 1 {
			t.Fatalf("remove = %d %s, removals %v", rec.Code, rec.Body, e.store.removals)
		}
	})

	t.Run("nothing to remove", func(t *testing.T) {
		e := newChartEnv(t)
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body)
		}
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=perhaps", nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("keepValues=perhaps = %d", rec.Code)
		}
	})

	t.Run("a Secret that cannot be kept stops the removal", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, true)
		e.kube.Forbidden["secrets"] = true
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=true", nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body)
		}
		if e.kube.Object(addonPlural, "example") == nil || len(e.store.removals) != 0 {
			t.Error("the addon must stay when its values cannot be kept")
		}
	})
}

// The whole lifecycle, watched from the apiserver: portal-api writes secret
// inputs with create, patch and delete only, and no answer carries a value.
func TestChartLifecycleNeverReadsSecrets(t *testing.T) {
	e := newChartEnv(t)
	const value = "a-very-secret-input"
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	check := func(rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, want)
		}
		if body := rec.Body.String(); strings.Contains(body, value) || strings.Contains(body, encoded) {
			t.Fatalf("an answer carries a secret value: %s", body)
		}
	}
	check(e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{
		"chart": "oci://registry.example.org/charts/example", "version": "1.2.0",
		"secretValues": map[string]string{"config.password": value},
	}), http.StatusAccepted)
	e.plans("example", "Planned", examplePlan("1.2.0"), nil)
	check(e.do(http.MethodGet, "/api/portal/addon-charts/example", nil), http.StatusOK)
	check(e.do(http.MethodPost, "/api/portal/addon-charts/example/install", nil), http.StatusAccepted)
	check(e.do(http.MethodPatch, "/api/portal/addon-charts/example", map[string]any{"secretValues": map[string]string{"config.password": value + "2"}}), http.StatusAccepted)
	check(e.do(http.MethodPatch, "/api/portal/addon-charts/example", map[string]any{"clearSecrets": []string{"config.password"}}), http.StatusAccepted)
	check(e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=true", nil), http.StatusOK)

	for _, c := range secretCalls(e.kube) {
		if method := strings.Fields(c)[0]; method != http.MethodPost && method != http.MethodPatch && method != http.MethodDelete {
			t.Errorf("portal-api made %s against a Secret", c)
		}
	}
}
