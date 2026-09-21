package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// The operator console's writes answer with what they produced, because a
// caller that waits has nothing else to wait on. These tests pin the wire
// contract: the status codes, and the fields in the bodies.

type opEnv struct {
	t    *testing.T
	kube *k8sfake.Server
	h    http.Handler
}

func newOpEnv(t *testing.T, protected ...string) *opEnv {
	t.Helper()
	kube := k8sfake.New(t)
	cfg := config.Config{
		OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", OperatorPlural: "zaentrums",
		AdminRole: "zaentrum-admin", ProtectedNames: protected,
	}
	a := &API{addons: newFakeStore(), cfg: cfg, op: operator.New(kube.Client("zaentrum"), cfg)}
	a.registration.kick = make(chan struct{}, 1)
	jwt, err := auth.NewJWTVerifier(context.Background(), "", "", cfg.AdminRole, false, true)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	a.Register(r, auth.NewMiddleware(jwt, cfg.AdminRole, "zaentrum-addon"))
	return &opEnv{t: t, kube: kube, h: r}
}

func (e *opEnv) do(method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		rdr = bytes.NewReader(raw)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest(method, target, rdr))
	return rec
}

// decodeBody reads a JSON answer, failing the test when it is not one.
func (e *opEnv) decodeBody(rec *httptest.ResponseRecorder) map[string]any {
	e.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		e.t.Fatalf("answer is not JSON: %v (%q)", err, rec.Body.String())
	}
	return m
}

func (e *opEnv) putDeployment(name string, replicas int, owned bool) {
	md := map[string]any{"name": name}
	if owned {
		md["ownerReferences"] = []any{map[string]any{"kind": "Zaentrum", "name": "zaentrum"}}
	}
	e.kube.Put("deployments", map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": md,
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{k8s.RestartedAtAnnotation: "2026-09-21T08:00:00Z"}},
				"spec": map[string]any{"containers": []any{
					map[string]any{"name": name, "image": "ghcr.io/example/" + name + ":1.4.0"},
				}},
			},
		},
	})
	e.kube.SetStatus("deployments", name, map[string]any{
		"observedGeneration": 1, "replicas": replicas, "readyReplicas": replicas,
		"updatedReplicas": replicas, "availableReplicas": replicas,
	})
}

func (e *opEnv) putCR(availableUpdate string) {
	e.kube.Put("zaentrums", map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum"},
		"spec": map[string]any{"version": "1.4.0", "channel": "stable",
			"update": map[string]any{"mode": "manual"}},
	})
	e.kube.SetStatus("zaentrums", "zaentrum", map[string]any{
		"phase": "Ready", "currentVersion": "1.4.0", "availableUpdate": availableUpdate,
		"observedGeneration": 1,
	})
}

func TestOperatorGetCarriesTheRolloutFields(t *testing.T) {
	e := newOpEnv(t)
	e.putDeployment("chino-api", 2, true)
	e.putCR("1.5.0")

	rec := e.do(http.MethodGet, "/api/portal/operator", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET operator = %d %s", rec.Code, rec.Body)
	}
	var state struct {
		Operator  operator.OperatorInfo `json:"operator"`
		Instances []operator.Instance   `json:"instances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Instances) != 1 {
		t.Fatalf("instances = %+v", state.Instances)
	}
	i := state.Instances[0]
	if i.Generation != 1 || i.ObservedGeneration != 1 || i.RestartedAt != "2026-09-21T08:00:00Z" {
		t.Errorf("workload rollout fields = %d/%d %q", i.Generation, i.ObservedGeneration, i.RestartedAt)
	}
	if state.Operator.Generation != 1 || state.Operator.ObservedGeneration != 1 {
		t.Errorf("operator generations = %d/%d", state.Operator.Generation, state.Operator.ObservedGeneration)
	}
	// The fields the SPA and older CLIs read are untouched.
	for _, field := range []string{`"desiredReplicas"`, `"readyReplicas"`, `"phase"`, `"protected"`, `"operatorManaged"`, `"group"`} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("the existing field %s must stay", field)
		}
	}
}

func TestInstanceWritesAnswerWithTheirGeneration(t *testing.T) {
	e := newOpEnv(t)
	e.putDeployment("chino-api", 2, false)

	rec := e.do(http.MethodPost, "/api/portal/operator/instances/chino-api/restart", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body)
	}
	body := e.decodeBody(rec)
	if body["name"] != "chino-api" || body["generation"] != float64(2) {
		t.Fatalf("restart answered %v, want chino-api at generation 2", body)
	}
	if s, _ := body["restartedAt"].(string); s == "" {
		t.Errorf("a restart must answer with the stamp it wrote: %v", body)
	}

	rec = e.do(http.MethodPost, "/api/portal/operator/instances/chino-api/scale", map[string]any{"replicas": 3})
	if rec.Code != http.StatusOK {
		t.Fatalf("scale = %d %s", rec.Code, rec.Body)
	}
	if body = e.decodeBody(rec); body["generation"] != float64(3) {
		t.Fatalf("scale answered %v, want generation 3", body)
	}
	// The body a client may send is still exactly the one field.
	rec = e.do(http.MethodPost, "/api/portal/operator/instances/chino-api/scale", `{"replicas":2,"force":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field must still be refused: %d %s", rec.Code, rec.Body)
	}
}

func TestWritingToAMissingWorkloadIs404(t *testing.T) {
	e := newOpEnv(t, "postgres")
	e.putDeployment("chino-api", 1, false)

	for _, target := range []string{
		"/api/portal/operator/instances/nosuch/restart",
		"/api/portal/operator/instances/nosuch/scale",
	} {
		body := any(nil)
		if strings.HasSuffix(target, "scale") {
			body = map[string]any{"replicas": 1}
		}
		if rec := e.do(http.MethodPost, target, body); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d %s, want 404", target, rec.Code, rec.Body)
		}
	}

	// Protected keeps its refusal, and its reason: 400, not 404. The two mean
	// opposite things — "fix the name" and "the platform will not do that".
	rec := e.do(http.MethodPost, "/api/portal/operator/instances/postgres/restart", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "protected (stateful) service") {
		t.Errorf("protected restart = %d %s, want 400 with its reason", rec.Code, rec.Body)
	}
}

func TestPatchOperatorAnswersWithVersionAndGeneration(t *testing.T) {
	e := newOpEnv(t)
	e.putCR("1.5.0")

	rec := e.do(http.MethodPatch, "/api/portal/operator", map[string]any{"version": "1.6.0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH operator = %d %s", rec.Code, rec.Body)
	}
	body := e.decodeBody(rec)
	if body["version"] != "1.6.0" || body["generation"] != float64(2) {
		t.Fatalf("PATCH answered %v, want 1.6.0 at generation 2", body)
	}
	// Nothing to change is still a refusal, not a silent success.
	if rec = e.do(http.MethodPatch, "/api/portal/operator", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty patch = %d, want 400", rec.Code)
	}
}

func TestApplyUpdateTakesAnOptionalExpectedVersion(t *testing.T) {
	// No body at all: apply whatever is on the shelf.
	e := newOpEnv(t)
	e.putCR("1.5.0")
	rec := e.do(http.MethodPost, "/api/portal/operator/apply-update", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply-update with no body = %d %s", rec.Code, rec.Body)
	}
	if body := e.decodeBody(rec); body["version"] != "1.5.0" || body["generation"] != float64(2) {
		t.Fatalf("apply-update answered %v", body)
	}

	// The version the caller decided on, and it still matches.
	e2 := newOpEnv(t)
	e2.putCR("1.5.0")
	rec = e2.do(http.MethodPost, "/api/portal/operator/apply-update", map[string]any{"version": "1.5.0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply-update with the matching version = %d %s", rec.Code, rec.Body)
	}

	// It moved while the caller was deciding: 409, and the answer says what is
	// there now — that is the whole point of sending it.
	e3 := newOpEnv(t)
	e3.putCR("2.0.0-rc1")
	rec = e3.do(http.MethodPost, "/api/portal/operator/apply-update", map[string]any{"version": "1.5.0"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("apply-update with a stale version = %d %s, want 409", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "2.0.0-rc1") {
		t.Errorf("the conflict must name what is on the shelf now: %s", rec.Body)
	}
	if v, _ := e3.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any)["version"].(string); v != "1.4.0" {
		t.Errorf("a refused apply must write nothing: spec.version = %q", v)
	}

	// Nothing discovered is a refusal, not a conflict.
	e4 := newOpEnv(t)
	e4.putCR("")
	if rec = e4.do(http.MethodPost, "/api/portal/operator/apply-update", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("no update available = %d %s, want 400", rec.Code, rec.Body)
	}

	// A body that is sent must still be a well-formed one.
	e5 := newOpEnv(t)
	e5.putCR("1.5.0")
	if rec = e5.do(http.MethodPost, "/api/portal/operator/apply-update", `{"channel":"edge"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field = %d, want 400", rec.Code)
	}
}
