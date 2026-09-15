package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
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
	cfg := config.Config{
		OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", OperatorPlural: "zaentrums",
		AddonPlural: addonPlural, AdminRole: "zaentrum-admin",
	}
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

// valuesFrom is an addon's spec.valuesFrom as JSON, fields sorted.
func (e *chartEnv) valuesFrom(name string) string {
	e.t.Helper()
	b, _ := json.Marshal(e.spec(name)["valuesFrom"])
	return string(b)
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

// secretsCreated lists the Secrets portal-api created, in the order it did.
func (e *chartEnv) secretsCreated() []string {
	var out []string
	for _, c := range e.kube.Calls() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/secrets") {
			out = append(out, c.Body)
		}
	}
	return out
}

// writes is the order of the writes portal-api made: "dry-run" and "addon"
// for ZaentrumAddon creates and updates, "secret" for a Secret.
func (e *chartEnv) writes() []string {
	var out []string
	for _, c := range e.kube.Calls() {
		switch {
		case c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/secrets"):
			out = append(out, "secret")
		case (c.Method == http.MethodPost || c.Method == http.MethodPut) && strings.Contains(c.Path, addonPlural):
			if c.Query == "dryRun=All" {
				out = append(out, "dry-run")
			} else {
				out = append(out, "addon")
			}
		case c.Method == http.MethodPatch || c.Method == http.MethodDelete:
			out = append(out, strings.ToLower(c.Method))
		}
	}
	return out
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
		"objects": []any{
			map[string]any{"kind": "Service", "name": "example"},
			map[string]any{"kind": "Deployment", "name": "example"},
			map[string]any{"kind": "Deployment", "name": "example-worker"},
		},
		"workloads": []any{
			map[string]any{"kind": "Deployment", "name": "example", "images": []any{"ghcr.io/example/example:" + version}, "ports": []any{8080}},
			map[string]any{"kind": "Deployment", "name": "example-worker", "images": []any{"ghcr.io/example/example-worker:" + version}, "ports": []any{}},
		},
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// secretData decodes a Secret's data, for a test to inspect.
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

func decodeAccepted(t *testing.T, rec *httptest.ResponseRecorder) chartAccepted {
	t.Helper()
	var got chartAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return got
}

// entry is one valuesFrom entry in the order json.Marshal writes its fields.
func entry(name, path string) string {
	return fmt.Sprintf(`{"kind":"Secret","name":%q,"targetPath":%q,"valuesKey":%q}`, name, path, path)
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
		{"https://charts.example.org/dl/example-worker-v0.3.1-rc.1.tar.gz", "", "", operator.ChartSource{Ref: "https://charts.example.org/dl/example-worker-v0.3.1-rc.1.tar.gz"}, "example-worker"},
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
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if got := decodeAccepted(t, rec); got.Name != "example" || got.Generation != 1 || got.ObservedGeneration != 0 ||
		got.Chart != (operator.ChartSource{Ref: "oci://registry.example.org/charts/example", Version: "1.2.0"}) {
		t.Errorf("accepted = %+v — a client tells this plan from an older one by the generation", got)
	}
	spec := e.spec("example")
	chart := spec["chart"].(map[string]any)
	if chart["ref"] != "oci://registry.example.org/charts/example" || chart["version"] != "1.2.0" || spec["suspend"] != true {
		t.Errorf("spec = %v — a new addon is planned, not applied", spec)
	}
	if b, _ := json.Marshal(spec["values"]); string(b) != `{"worker":{"replicas":2}}` {
		t.Errorf("values = %s", b)
	}
	names := e.kube.SecretNames()
	if len(names) != 1 || !strings.HasPrefix(names[0], "zaentrum-addon-example-values-") {
		t.Fatalf("secrets = %v — one new Secret, named from the addon's generateName", names)
	}
	first := names[0]
	if got := e.valuesFrom("example"); got != "["+entry(first, "config.password")+"]" {
		t.Errorf("valuesFrom = %s", got)
	}
	sec := e.kube.Secret(first)
	labels := sec["metadata"].(map[string]any)["labels"].(map[string]any)
	if sec["immutable"] != true || sec["type"] != "Opaque" || labels["zaentrum.io/addon"] != "example" {
		t.Errorf("secret = %v — immutable, opaque, labelled for the addon", sec)
	}
	if got := secretData(t, e.kube, first); len(got) != 1 || got["config.password"] != "s3cret-value" {
		t.Errorf("secret data = %v", got)
	}
	// Validated by a dry run before the Secret existed; written after.
	if got := strings.Join(e.writes(), ","); got != "dry-run,secret,addon" {
		t.Errorf("writes = %s", got)
	}
	if strings.Contains(rec.Body.String(), "s3cret-value") {
		t.Error("an answer must never carry a secret value")
	}

	// Adding it again updates it: values replaced, planned again, and the new
	// input stored in a Secret of its own; the first stays as it is.
	e.plans("example", "Planned", examplePlan("1.2.0"), nil)
	rec = e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{
		"name": "example", "chart": "https://charts.example.org/example-1.3.0.tgz",
		"values":       map[string]any{"logLevel": "debug"},
		"secretValues": map[string]string{"database.url": "postgres://db.example.org/example"},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}
	if got := decodeAccepted(t, rec); got.Generation != 2 || got.ObservedGeneration != 1 || got.Chart.Ref != "https://charts.example.org/example-1.3.0.tgz" {
		t.Errorf("accepted after update = %+v", got)
	}
	spec = e.spec("example")
	if b, _ := json.Marshal(spec["values"]); string(b) != `{"logLevel":"debug"}` {
		t.Errorf("values after update = %s", b)
	}
	if chart := spec["chart"].(map[string]any); chart["ref"] != "https://charts.example.org/example-1.3.0.tgz" || chart["version"] != nil {
		t.Errorf("chart after update = %v — an archive link carries no version", chart)
	}
	names = e.kube.SecretNames()
	if len(names) != 2 {
		t.Fatalf("secrets = %v", names)
	}
	second := names[0]
	if second == first {
		second = names[1]
	}
	if got := e.valuesFrom("example"); got != "["+entry(first, "config.password")+","+entry(second, "database.url")+"]" {
		t.Errorf("valuesFrom after update = %s", got)
	}
	if got := secretData(t, e.kube, first); len(got) != 1 || got["config.password"] != "s3cret-value" {
		t.Errorf("the first Secret must be left as it was: %v", got)
	}
}

// Everything a request carries is checked before anything is stored: a
// refused request leaves neither a ZaentrumAddon nor a Secret.
func TestCreateAddonChartRefusals(t *testing.T) {
	chart := "oci://registry.example.org/charts/example:1.2.0"
	long := strings.Repeat("a", 125) + "." + strings.Repeat("b", 125) // 251 characters
	cases := []struct {
		name    string
		prepare func(*chartEnv)
		body    any
		want    int
		mention string
	}{
		{"no chart", nil, map[string]any{}, http.StatusBadRequest, "chart is required"},
		{"not json", nil, "{", http.StatusBadRequest, "invalid json"},
		{"derived name is no label", nil, map[string]any{"chart": "https://charts.example.org/Example_Chart-1.0.0.tgz", "secretValues": map[string]string{"a": "x"}}, http.StatusBadRequest, "DNS-1123"},
		{"name too long", nil, map[string]any{"chart": chart, "name": strings.Repeat("a", 41), "secretValues": map[string]string{"a": "x"}}, http.StatusBadRequest, "at most 40"},
		{"values not an object", nil, map[string]any{"chart": chart, "values": []int{1}, "secretValues": map[string]string{"a": "x"}}, http.StatusBadRequest, "JSON object"},
		{"values set the platform's key", nil, map[string]any{"chart": chart, "values": map[string]any{"zaentrum": map[string]any{"hostname": "x"}}}, http.StatusBadRequest, "reserved"},
		{"secret path with an empty segment", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"config..password": "x"}}, http.StatusBadRequest, "dotted values path"},
		{"secret path longer than the resource allows", nil, map[string]any{"chart": chart, "secretValues": map[string]string{long: "x"}}, http.StatusBadRequest, "at most 250"},
		{"secret path under the platform's key", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"zaentrum.issuer": "x"}}, http.StatusBadRequest, "reserved"},
		{"empty secret", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"config.password": ""}}, http.StatusBadRequest, "clearSecrets"},
		{"too many secret inputs", nil, map[string]any{"chart": chart, "secretValues": manyInputs("p", 65)}, http.StatusBadRequest, "at most 64"},
		{"a reference to a Secret of another addon", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"a": "x"},
			"secretRefs": map[string]any{"config.password": map[string]string{"name": "zaentrum-addon-other-values-abcde", "key": "config.password"}}}, http.StatusBadRequest, "zaentrum-addon-example-"},
		{"a reference with a key no Secret has", nil, map[string]any{"chart": chart,
			"secretRefs": map[string]any{"config.password": map[string]string{"name": "zaentrum-addon-example-values-abcde", "key": "a key"}}}, http.StatusBadRequest, "no key"},
		{"a path both set and referenced", nil, map[string]any{"chart": chart, "secretValues": map[string]string{"config.password": "x"},
			"secretRefs": map[string]any{"config.password": map[string]string{"name": "zaentrum-addon-example-values-abcde"}}}, http.StatusBadRequest, "both set and referenced"},
		{"name of an addon added by address", func(e *chartEnv) {
			e.store.addons["example"] = model.Addon{Key: "example", Address: "http://example"}
		}, map[string]any{"chart": chart, "secretValues": map[string]string{"a": "x"}}, http.StatusConflict, "http://example"},
		{"key of an app registered by hand", func(e *chartEnv) {
			e.store.apps["example"] = model.App{Key: "example", Title: "example"}
		}, map[string]any{"chart": chart, "secretValues": map[string]string{"a": "x"}}, http.StatusConflict, "already registered"},
		{"a spec the apiserver refuses", func(e *chartEnv) {
			e.kube.Validate = func(string, map[string]any) error {
				return errors.New(`ZaentrumAddon.zaentrum.io "example" is invalid: spec.chart.ref: Invalid value`)
			}
		}, map[string]any{"chart": chart, "secretValues": map[string]string{"config.password": "x"}}, http.StatusUnprocessableEntity, "spec.chart.ref"},
		{"cluster without the resource type", func(e *chartEnv) {
			e.kube.Unserved[addonPlural] = true
		}, map[string]any{"chart": chart, "secretValues": map[string]string{"config.password": "x"}}, http.StatusServiceUnavailable, "update the zaentrum-operator"},
		{"role without the resource", func(e *chartEnv) {
			e.kube.Forbidden[addonPlural] = true
		}, map[string]any{"chart": chart, "secretValues": map[string]string{"a": "x"}}, http.StatusServiceUnavailable, "Role"},
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
			if e.kube.Object(addonPlural, "example") != nil || len(e.secretsCreated()) != 0 {
				t.Errorf("a refused request must store nothing: secrets %v", e.kube.SecretNames())
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

func manyInputs(prefix string, n int) map[string]string {
	out := map[string]string{}
	for i := 0; i < n; i++ {
		out[fmt.Sprintf("%s.k%d", prefix, i)] = "v"
	}
	return out
}

// Another addon's chart rendered a Secret under the name an addon's values
// Secret once had, and controls it. Adding that addon stores its inputs in a
// Secret of its own: the squatted one is never written to.
func TestSquattedValuesSecretIsNeverWrittenTo(t *testing.T) {
	e := newChartEnv(t)
	e.kube.PutSecret(map[string]any{
		"metadata": map[string]any{
			"name":            "zaentrum-addon-example-values",
			"labels":          map[string]any{"zaentrum.io/addon": "bar"},
			"ownerReferences": []any{map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "ZaentrumAddon", "name": "bar", "controller": true}},
		},
		"data": map[string]any{"mounted-by-bar": b64("x")},
	})
	rec := e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{
		"chart":        "oci://registry.example.org/charts/example:1.2.0",
		"secretValues": map[string]string{"config.password": "victim-secret"},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if got := secretData(t, e.kube, "zaentrum-addon-example-values"); len(got) != 1 || got["mounted-by-bar"] != "x" {
		t.Errorf("the squatted Secret changed: %v", got)
	}
	inputs := e.api.chartsInputs(t, "example")
	if ref := inputs["config.password"]; ref.Name == "zaentrum-addon-example-values" || secretData(t, e.kube, ref.Name)["config.password"] != "victim-secret" {
		t.Errorf("the input must live in a new Secret: %+v", inputs)
	}
}

// chartsInputs reads an addon's secret inputs through portal-api's own client.
func (a *API) chartsInputs(t *testing.T, name string) map[string]operator.SecretRef {
	t.Helper()
	ca, err := a.charts.ChartAddon(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return ca.SecretInputs()
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
			map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values-abcde", "valuesKey": "config.password", "targetPath": "config.password"},
			map[string]any{"kind": "Secret", "name": "gitops-token", "valuesKey": "token", "targetPath": "config.token"},
			map[string]any{"kind": "Secret", "name": "shared-document"},
		},
	}, nil)
	plan := examplePlan("1.2.0")
	plan["valuesErrors"] = []any{"config.password: required"}
	e.plans("example", "PlanFailed", plan, map[string]any{"message": "values do not validate"})
	e.kube.PutSecret(map[string]any{"metadata": map[string]any{"name": "zaentrum-addon-example-values-abcde"}, "data": map[string]any{"config.password": b64("s3cret-value")}})

	rec := e.do(http.MethodGet, "/api/portal/addon-charts/example", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), b64("s3cret-value")) || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatal("a secret value must never appear in an answer")
	}
	var got struct {
		Name        string                        `json:"name"`
		Suspended   bool                          `json:"suspended"`
		Phase       string                        `json:"phase"`
		Message     string                        `json:"message"`
		Plan        map[string]any                `json:"plan"`
		Components  []any                         `json:"components"`
		LastApplied *operator.ChartSource         `json:"lastAppliedChart"`
		Values      map[string]any                `json:"values"`
		SecretKeys  []string                      `json:"secretKeys"`
		SecretRefs  map[string]operator.SecretRef `json:"secretRefs"`
		Registered  bool                          `json:"registered"`
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
	// Every value a Secret sets, wherever it was declared — not a values
	// document, and nothing from a ConfigMap.
	if strings.Join(got.SecretKeys, ",") != "config.password,config.token" ||
		got.SecretRefs["config.password"] != (operator.SecretRef{Name: "zaentrum-addon-example-values-abcde", Key: "config.password"}) ||
		got.SecretRefs["config.token"] != (operator.SecretRef{Name: "gitops-token", Key: "token"}) {
		t.Errorf("secretKeys = %v, secretRefs = %v", got.SecretKeys, got.SecretRefs)
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
		rec := install(e)
		if rec.Code != http.StatusAccepted || suspended(e) {
			t.Fatalf("install = %d %s, suspend=%v", rec.Code, rec.Body, e.spec("example")["suspend"])
		}
		if got := decodeAccepted(t, rec); got.Generation != 2 || got.ObservedGeneration != 1 {
			t.Errorf("accepted = %+v — suspend is spec: installing moves the generation", got)
		}
		select {
		case <-e.api.registration.kick:
		default:
			t.Error("install must kick the registration loop")
		}
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

// installedExample seeds an applied example addon whose password is stored in
// a Secret, next to entries portal-api does not manage.
func installedExample(e *chartEnv, digest string) {
	e.seed("example", map[string]any{
		"chart":   map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0", "digest": digest},
		"values":  map[string]any{"worker": map[string]any{"replicas": 2}},
		"suspend": false,
		"valuesFrom": []any{
			map[string]any{"kind": "ConfigMap", "name": "shared-values"},
			map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values-old", "valuesKey": "config.password", "targetPath": "config.password"},
		},
	}, map[string]any{"phase": "Ready", "lastAppliedChart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}})
	e.kube.PutSecret(map[string]any{
		"metadata": map[string]any{"name": "zaentrum-addon-example-values-old", "labels": map[string]any{"zaentrum.io/addon": "example"}},
		"data":     map[string]any{"config.password": b64("old")},
	})
}

func TestPatchAddonChart(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	patch := func(t *testing.T, e *chartEnv, body any) chartAccepted {
		t.Helper()
		rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("patch %v = %d %s", body, rec.Code, rec.Body)
		}
		return decodeAccepted(t, rec)
	}

	t.Run("upgrade plans first", func(t *testing.T) {
		e := newChartEnv(t)
		installedExample(e, digest)
		if got := patch(t, e, map[string]any{"version": "1.3.0"}); got.Generation != 2 || got.Chart.Version != "1.3.0" || got.Chart.Digest != "" {
			t.Errorf("accepted = %+v", got)
		}
		spec := e.spec("example")
		chart := spec["chart"].(map[string]any)
		if chart["version"] != "1.3.0" || chart["digest"] != nil || spec["suspend"] != true {
			t.Errorf("spec = %v — a new version drops the old digest and is planned first", spec)
		}
		if spec["values"] == nil || len(spec["valuesFrom"].([]any)) != 2 {
			t.Errorf("an upgrade keeps values and inputs: %v", spec)
		}
		patch(t, e, map[string]any{"version": "1.4.0", "digest": digest, "suspend": false})
		spec = e.spec("example")
		if chart := spec["chart"].(map[string]any); chart["version"] != "1.4.0" || chart["digest"] != digest || spec["suspend"] != false {
			t.Errorf("spec = %v", spec)
		}
		patch(t, e, map[string]any{"chart": "oci://registry.example.org/charts/example:2.0.0"})
		if chart := e.spec("example")["chart"].(map[string]any); chart["version"] != "2.0.0" || chart["ref"] != "oci://registry.example.org/charts/example" {
			t.Errorf("chart = %v", chart)
		}
		if len(e.secretsCreated()) != 0 {
			t.Error("an upgrade stores no Secret")
		}
	})

	t.Run("reconfigure applies in place", func(t *testing.T) {
		e := newChartEnv(t)
		installedExample(e, "")
		gen := e.kube.Generation(addonPlural, "example")
		patch(t, e, map[string]any{
			"values":       map[string]any{"logLevel": "debug"},
			"secretValues": map[string]string{"database.url": "postgres://db.example.org/example"},
			"clearSecrets": []string{"config.password"},
		})
		spec := e.spec("example")
		if spec["suspend"] != false || e.kube.Generation(addonPlural, "example") != gen+1 {
			t.Errorf("values alone must not suspend an installed addon, and must move the generation: %v", spec)
		}
		if b, _ := json.Marshal(spec["values"]); string(b) != `{"logLevel":"debug"}` {
			t.Errorf("values = %s", b)
		}
		names := e.kube.SecretNames()
		if len(names) != 2 {
			t.Fatalf("secrets = %v", names)
		}
		created := names[0]
		if created == "zaentrum-addon-example-values-old" {
			created = names[1]
		}
		if got := e.valuesFrom("example"); got != `[{"kind":"ConfigMap","name":"shared-values"},`+entry(created, "database.url")+`]` {
			t.Errorf("valuesFrom = %s — other entries stay first, in place", got)
		}
		if got := secretData(t, e.kube, created); len(got) != 1 || got["database.url"] == "" {
			t.Errorf("new secret = %v", got)
		}
		if got := secretData(t, e.kube, "zaentrum-addon-example-values-old"); got["config.password"] != "old" {
			t.Errorf("a cleared input's Secret is the operator's to collect, not portal-api's to change: %v", got)
		}
		if got := strings.Join(e.writes(), ","); got != "dry-run,secret,addon" {
			t.Errorf("writes = %s", got)
		}
		patch(t, e, map[string]any{"values": nil})
		if _, ok := e.spec("example")["values"]; ok {
			t.Error("values: null removes them")
		}
	})

	t.Run("refusals store nothing", func(t *testing.T) {
		e := newChartEnv(t)
		installedExample(e, "")
		for _, c := range []struct {
			body    any
			want    int
			mention string
		}{
			{map[string]any{"secretValues": map[string]string{"a": "x"}, "clearSecrets": []string{"a"}}, http.StatusBadRequest, "both set and cleared"},
			{map[string]any{"version": "", "secretValues": map[string]string{"config.password": "new"}}, http.StatusBadRequest, "version is required"},
			{map[string]any{"version": "not a version", "secretValues": map[string]string{"config.password": "new"}}, http.StatusBadRequest, "not a valid chart version"},
			{map[string]any{"chart": "http://charts.example.org/example.tgz", "secretValues": map[string]string{"config.password": "new"}}, http.StatusBadRequest, "https://"},
			{map[string]any{"clearSecrets": []string{"../x"}}, http.StatusBadRequest, "dotted values path"},
			{map[string]any{"secretValues": manyInputs("new", 64), "secretRefs": map[string]any{"one.more": map[string]string{"name": "zaentrum-addon-example-values-kept"}}}, http.StatusBadRequest, "at most 64"},
		} {
			rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", c.body)
			if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.mention) {
				t.Errorf("patch %v = %d %s, want %d mentioning %q", c.body, rec.Code, rec.Body, c.want, c.mention)
			}
		}
		// 40 inputs the addon has, 40 more in one request: over the limit only
		// together — found before a Secret is stored.
		from := []any{}
		for i := 0; i < 40; i++ {
			p := fmt.Sprintf("old.k%d", i)
			from = append(from, map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values-old", "valuesKey": p, "targetPath": p})
		}
		e.seed("sample", map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/sample", "version": "1.0.0"}, "valuesFrom": from}, nil)
		if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/sample", map[string]any{"secretValues": manyInputs("new", 40)}); rec.Code != http.StatusBadRequest {
			t.Errorf("patch past the limit = %d %s", rec.Code, rec.Body)
		}
		if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/other", map[string]any{"secretValues": map[string]string{"a": "x"}}); rec.Code != http.StatusNotFound {
			t.Errorf("patch of a missing addon = %d %s", rec.Code, rec.Body)
		}
		if len(e.secretsCreated()) != 0 || e.kube.Generation(addonPlural, "example") != 1 {
			t.Errorf("refused patches must store nothing: secrets %v, generation %d", e.kube.SecretNames(), e.kube.Generation(addonPlural, "example"))
		}
		if got := secretData(t, e.kube, "zaentrum-addon-example-values-old"); got["config.password"] != "old" {
			t.Errorf("the live input changed: %v", got)
		}
	})
}

// A write whose resource update fails after its Secret was stored: the addon
// keeps reading the old input — the running addon never sees the new value —
// and the answer says which Secret was left behind.
func TestFailedWriteReportsItsOrphanedSecret(t *testing.T) {
	t.Run("patch", func(t *testing.T) {
		e := newChartEnv(t)
		installedExample(e, "")
		e.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
			if c.Method == http.MethodPut && c.Query == "" && strings.Contains(c.Path, addonPlural) {
				return http.StatusConflict, "the object has been modified; please apply your changes to the latest version and try again", true
			}
			return 0, "", false
		}
		rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", map[string]any{"secretValues": map[string]string{"config.password": "new"}})
		names := e.kube.SecretNames()
		if rec.Code != http.StatusConflict || len(names) != 2 {
			t.Fatalf("patch = %d %s, secrets %v", rec.Code, rec.Body, names)
		}
		orphan := names[0]
		if orphan == "zaentrum-addon-example-values-old" {
			orphan = names[1]
		}
		if !strings.Contains(rec.Body.String(), orphan) || !strings.Contains(rec.Body.String(), "nothing references") {
			t.Errorf("the answer must name the Secret left behind: %s", rec.Body)
		}
		if ref := e.api.chartsInputs(t, "example")["config.password"]; ref.Name != "zaentrum-addon-example-values-old" {
			t.Errorf("the addon must still read the old input: %+v", ref)
		}
		if got := secretData(t, e.kube, "zaentrum-addon-example-values-old"); got["config.password"] != "old" {
			t.Errorf("the live input changed: %v", got)
		}
	})
	t.Run("create", func(t *testing.T) {
		e := newChartEnv(t)
		e.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
			if c.Method == http.MethodPost && c.Query == "" && strings.HasSuffix(c.Path, addonPlural) {
				return http.StatusInternalServerError, "etcdserver: request timed out", true
			}
			return 0, "", false
		}
		rec := e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{
			"chart": "oci://registry.example.org/charts/example:1.2.0", "secretValues": map[string]string{"config.password": "x"},
		})
		names := e.kube.SecretNames()
		if rec.Code != http.StatusBadGateway || len(names) != 1 || !strings.Contains(rec.Body.String(), names[0]) {
			t.Errorf("create = %d %s, secrets %v", rec.Code, rec.Body, names)
		}
	})
}

// Replacing an input's value points its entry at a new Secret: the spec
// changes, so the plan made with the old value no longer counts as current.
func TestSecretWriteMakesThePlanStale(t *testing.T) {
	e := newChartEnv(t)
	e.seed("example", map[string]any{
		"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}, "suspend": true,
		"valuesFrom": []any{map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values-old", "valuesKey": "config.password", "targetPath": "config.password"}},
	}, nil)
	e.plans("example", "Planned", examplePlan("1.2.0"), nil)
	gen := e.kube.Generation(addonPlural, "example")
	if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", map[string]any{"secretValues": map[string]string{"config.password": "a value the plan never saw"}}); rec.Code != http.StatusAccepted {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	if after := e.kube.Generation(addonPlural, "example"); after != gen+1 {
		t.Errorf("generation %d → %d, want a new one", gen, after)
	}
	if inst := e.do(http.MethodPost, "/api/portal/addon-charts/example/install", nil); inst.Code != http.StatusConflict {
		t.Errorf("install on the old plan = %d %s, want 409", inst.Code, inst.Body)
	}
}

// A values change through the portal leaves every valuesFrom entry it does not
// touch exactly as a deploy repository wrote it — place, name and fields.
func TestPatchLeavesOtherValuesFromEntriesAlone(t *testing.T) {
	e := newChartEnv(t)
	e.seed("example", map[string]any{
		"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}, "suspend": false,
		"valuesFrom": []any{
			map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values", "valuesKey": "config.token", "targetPath": "config.token", "optional": true},
			map[string]any{"kind": "Secret", "name": "shared", "valuesKey": "values.yaml", "optional": true},
		},
	}, map[string]any{"phase": "Ready"})
	before := e.valuesFrom("example")
	if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", map[string]any{"values": map[string]any{"logLevel": "debug"}}); rec.Code != http.StatusAccepted {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	if after := e.valuesFrom("example"); after != before {
		t.Errorf("valuesFrom\n%s\nbecame\n%s", before, after)
	}
	// An input set through the portal keeps its entry's place and fields.
	if rec := e.do(http.MethodPatch, "/api/portal/addon-charts/example", map[string]any{"secretValues": map[string]string{"config.token": "t"}}); rec.Code != http.StatusAccepted {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	name := e.api.chartsInputs(t, "example")["config.token"].Name
	want := fmt.Sprintf(`[{"kind":"Secret","name":%q,"optional":true,"targetPath":"config.token","valuesKey":"config.token"},{"kind":"Secret","name":"shared","optional":true,"valuesKey":"values.yaml"}]`, name)
	if after := e.valuesFrom("example"); after != want {
		t.Errorf("valuesFrom = %s\nwant %s", after, want)
	}
}

func TestRemoveAddonChart(t *testing.T) {
	prepare := func(e *chartEnv, registered bool) {
		e.seed("example", map[string]any{"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.2.0"}}, nil)
		e.kube.PutSecret(map[string]any{
			"metadata": map[string]any{"name": "zaentrum-addon-example-values-abcde", "labels": map[string]any{"zaentrum.io/addon": "example"}},
			"data":     map[string]any{"config.password": b64("s3cret")},
		})
		if registered {
			e.store.addons["example"] = model.Addon{Key: "example", ChartRef: "oci://registry.example.org/charts/example", ChartVersion: "1.2.0"}
		}
	}
	annotation := func(obj map[string]any) any {
		md, _ := obj["metadata"].(map[string]any)
		annotations, _ := md["annotations"].(map[string]any)
		return annotations["zaentrum.io/keep-values"]
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
		// The operator's finalizer reads the annotation on the resource it deletes.
		if got := annotation(e.kube.Deleted(addonPlural, "example")); got != "true" {
			t.Errorf("keep-values on the deleted resource = %v", got)
		}
		if got := strings.Join(e.writes(), ","); got != "patch,delete" {
			t.Errorf("writes = %s — the annotation first, then the delete, and no Secret touched", got)
		}
		if len(e.store.removals) != 1 || e.store.removals[0] != "example|" {
			t.Errorf("registry rows must go: %v", e.store.removals)
		}
	})

	t.Run("without keeping values", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, true)
		rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example", nil)
		if rec.Code != http.StatusOK || e.kube.Object(addonPlural, "example") != nil || len(e.store.removals) != 1 {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body)
		}
		if got := strings.Join(e.writes(), ","); got != "delete" {
			t.Errorf("writes = %s", got)
		}
	})

	t.Run("an annotation an earlier attempt left is taken away", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, false)
		obj := e.kube.Object(addonPlural, "example")
		obj["metadata"].(map[string]any)["annotations"] = map[string]any{"zaentrum.io/keep-values": "true"}
		e.kube.Put(addonPlural, obj)
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=false", nil); rec.Code != http.StatusOK {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body)
		}
		if got := annotation(e.kube.Deleted(addonPlural, "example")); got != nil {
			t.Errorf("keep-values on the deleted resource = %v, want none", got)
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

	t.Run("the annotation cannot be set: the addon stays", func(t *testing.T) {
		e := newChartEnv(t)
		prepare(e, true)
		e.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
			return http.StatusForbidden, "zaentrumaddons is forbidden", c.Method == http.MethodPatch
		}
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=true", nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("remove = %d %s", rec.Code, rec.Body)
		}
		if e.kube.Object(addonPlural, "example") == nil || len(e.store.removals) != 0 {
			t.Error("the addon must stay when its values cannot be kept")
		}
	})

	t.Run("nothing to remove", func(t *testing.T) {
		e := newChartEnv(t)
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=perhaps", nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("keepValues=perhaps = %d", rec.Code)
		}
		// Rows of a resource deleted elsewhere are the registration loop's to remove.
		e.store.addons["example"] = model.Addon{Key: "example", ChartRef: "oci://registry.example.org/charts/example"}
		if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example", nil); rec.Code != http.StatusNotFound || len(e.store.removals) != 0 {
			t.Fatalf("remove = %d %s, removals %v", rec.Code, rec.Body, e.store.removals)
		}
	})
}

// A name without a ZaentrumAddon is answered 404 before anything happens —
// not a Secret touched, whoever's it is.
func TestRemoveOfAMissingAddonTouchesNothing(t *testing.T) {
	for _, target := range []string{"/api/portal/addon-charts/example", "/api/portal/addon-charts/example?keepValues=true"} {
		e := newChartEnv(t)
		for _, n := range []string{"zaentrum-addon-example-values", "zaentrum-addon-example-generated"} {
			e.kube.PutSecret(map[string]any{
				"metadata": map[string]any{
					"name":            n,
					"labels":          map[string]any{"zaentrum.io/addon": "bar"},
					"ownerReferences": []any{map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "ZaentrumAddon", "name": "bar", "controller": true}},
				},
				"data": map[string]any{"k": b64("v")},
			})
		}
		rec := e.do(http.MethodDelete, target, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d %s", target, rec.Code, rec.Body)
		}
		if w := e.writes(); len(w) != 0 {
			t.Errorf("%s wrote %v", target, w)
		}
		for _, n := range []string{"zaentrum-addon-example-values", "zaentrum-addon-example-generated"} {
			if refs := e.kube.Secret(n)["metadata"].(map[string]any)["ownerReferences"]; refs == nil {
				t.Errorf("%s: %s lost its owner", target, n)
			}
		}
	}
}

// Removed with keepValues and added again, an addon reads its kept inputs
// when the request references them.
func TestKeptValuesAreReferencedOnReAdd(t *testing.T) {
	e := newChartEnv(t)
	chart := "oci://registry.example.org/charts/example:1.2.0"
	if rec := e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{"chart": chart, "secretValues": map[string]string{"config.password": "kept"}}); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	kept := e.api.chartsInputs(t, "example")["config.password"]
	if rec := e.do(http.MethodDelete, "/api/portal/addon-charts/example?keepValues=true", nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	rec := e.do(http.MethodPost, "/api/portal/addon-charts", map[string]any{
		"chart":      chart,
		"secretRefs": map[string]any{"config.password": map[string]string{"name": kept.Name, "key": "config.password"}},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("re-add = %d %s", rec.Code, rec.Body)
	}
	get := e.do(http.MethodGet, "/api/portal/addon-charts/example", nil)
	var view struct {
		SecretKeys []string                      `json:"secretKeys"`
		SecretRefs map[string]operator.SecretRef `json:"secretRefs"`
	}
	_ = json.Unmarshal(get.Body.Bytes(), &view)
	if strings.Join(view.SecretKeys, ",") != "config.password" || view.SecretRefs["config.password"] != kept {
		t.Errorf("re-added: %+v, want the kept %+v", view, kept)
	}
	if len(e.secretsCreated()) != 1 {
		t.Errorf("a reference stores no Secret: %d created", len(e.secretsCreated()))
	}
}

// The lifecycle, watched from the apiserver: portal-api only ever creates
// Secrets, and no answer carries a value.
func TestChartLifecycleOnlyCreatesSecrets(t *testing.T) {
	e := newChartEnv(t)
	const value = "a-very-secret-input"
	check := func(rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, want)
		}
		if body := rec.Body.String(); strings.Contains(body, value) || strings.Contains(body, b64(value)) {
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

	for _, c := range e.kube.Calls() {
		if strings.Contains(c.Path, "/secrets") && c.Method != http.MethodPost {
			t.Errorf("portal-api made %s %s against a Secret", c.Method, c.Path)
		}
	}
	if n := len(e.secretsCreated()); n != 2 {
		t.Errorf("secrets created = %d, want one per write of secret inputs", n)
	}
}

// Every addon route is admin-only, and none answers without a bearer.
func TestChartRoutesAreAdminOnly(t *testing.T) {
	jwt, _ := auth.NewJWTVerifier(context.Background(), "", "", "zaentrum-admin", false, true)
	a := &API{addons: newFakeStore(), cfg: config.Config{AdminRole: "zaentrum-admin"}}
	r := chi.NewRouter()
	a.Register(r, auth.NewMiddleware(jwt, "zaentrum-admin", "zaentrum-addon"))
	routes := 0
	_ = chi.Walk(r, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if !strings.Contains(route, "/addon") {
			return nil
		}
		routes++
		admin := false
		for _, mw := range mws {
			if strings.Contains(runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name(), "RequireAdmin-fm") {
				admin = true
			}
		}
		if !admin {
			t.Errorf("%s %s is not behind RequireAdmin", method, route)
		}
		return nil
	})
	if routes < 9 {
		t.Errorf("walked %d addon routes", routes)
	}
	closed, _ := auth.NewJWTVerifier(context.Background(), "http://127.0.0.1:1/realms/none", "", "zaentrum-admin", false, false)
	r2 := chi.NewRouter()
	a.Register(r2, auth.NewMiddleware(closed, "zaentrum-admin", "zaentrum-addon"))
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/portal/addon-charts"}, {"POST", "/api/portal/addon-charts"}, {"GET", "/api/portal/addon-charts/example"},
		{"PATCH", "/api/portal/addon-charts/example"}, {"DELETE", "/api/portal/addon-charts/example"}, {"POST", "/api/portal/addon-charts/example/install"},
	} {
		rec := httptest.NewRecorder()
		r2.ServeHTTP(rec, httptest.NewRequest(c.m, c.p, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d without a bearer", c.m, c.p, rec.Code)
		}
	}
}
