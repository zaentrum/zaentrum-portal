package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	return newOpEnvAs(t, "zaentrum-admin", protected...)
}

// newOpEnvAs serves the console to a caller who holds role and nothing else;
// the console's own admin role stays zaentrum-admin.
func newOpEnvAs(t *testing.T, role string, protected ...string) *opEnv {
	t.Helper()
	kube := k8sfake.New(t)
	cfg := config.Config{
		OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", OperatorPlural: "zaentrums",
		AdminRole: "zaentrum-admin", ProtectedNames: protected,
		AdminStack: []string{"portal-api", "zaentrum-portal", "keycloak", "postgres", "katalog-manager-api"},
	}
	a := &API{addons: newFakeStore(), cfg: cfg, op: operator.New(kube.Client("zaentrum"), cfg)}
	a.registration.kick = make(chan struct{}, 1)
	// A verifier without an issuer signs every caller in with this one role.
	jwt, err := auth.NewJWTVerifier(context.Background(), "", "", role, false, true)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	a.Register(r, auth.NewMiddleware(jwt, auth.Policy{AdminRole: cfg.AdminRole, AddonRole: "zaentrum-addon"}))
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
	e.putDeploymentWith(name, replicas, owned, "")
}

// putDeploymentWith seeds a Deployment with an update strategy ("" for none
// named, which is RollingUpdate).
func (e *opEnv) putDeploymentWith(name string, replicas int, owned bool, strategy string) {
	md := map[string]any{"name": name}
	if owned {
		md["ownerReferences"] = []any{map[string]any{"kind": "Zaentrum", "name": "zaentrum"}}
	}
	e.kube.Put("deployments", map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": md,
		"spec": map[string]any{
			"replicas": replicas,
			"strategy": map[string]any{"type": strategy},
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
		"observedGeneration": 1, "replicas": replicas + 1, "readyReplicas": replicas,
		"updatedReplicas": replicas, "availableReplicas": replicas,
	})
}

func (e *opEnv) putCR(availableUpdate string) {
	e.putCRWithStatus(availableUpdate, nil)
}

// putCRWithStatus writes the CR with extra status fields folded in — the
// operator's own controller, when the operator reports one.
func (e *opEnv) putCRWithStatus(availableUpdate string, extra map[string]any) {
	e.kube.Put("zaentrums", map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum"},
		"spec": map[string]any{"version": "1.4.0", "channel": "stable",
			"update": map[string]any{"mode": "manual"}},
	})
	status := map[string]any{
		"phase": "Ready", "currentVersion": "1.4.0", "availableUpdate": availableUpdate,
		"observedGeneration": 1,
	}
	for k, v := range extra {
		status[k] = v
	}
	e.kube.SetStatus("zaentrums", "zaentrum", status)
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
	// status.replicas is the total across every ReplicaSet — here one more than
	// the two asked for, because the rollout is mid-surge.
	if i.Replicas != 3 || i.DesiredReplicas != 2 {
		t.Errorf("replicas/desiredReplicas = %d/%d, want 3/2", i.Replicas, i.DesiredReplicas)
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

// The operator's own controller reaches the console read, whole, and carries
// no write with it: the portal shows what is in charge and names where it is
// updated, and the update happens outside the product.
func TestOperatorGetCarriesTheController(t *testing.T) {
	e := newOpEnv(t)
	e.putCRWithStatus("1.5.0", map[string]any{"controller": map[string]any{
		"image":           "ghcr.io/example/operator:v0.4.1",
		"version":         "v0.4.1",
		"source":          "olm",
		"availableUpdate": "v0.5.0",
		"observedAt":      "2026-09-22T08:00:00Z",
	}})

	rec := e.do(http.MethodGet, "/api/portal/operator", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET operator = %d %s", rec.Code, rec.Body)
	}
	var state struct {
		Operator operator.OperatorInfo `json:"operator"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	c := state.Operator.Controller
	if c == nil {
		t.Fatalf("the controller must reach the console: %s", rec.Body)
	}
	if c.Image != "ghcr.io/example/operator:v0.4.1" || c.Version != "v0.4.1" ||
		c.Source != operator.SourceOLM || c.AvailableUpdate != "v0.5.0" || c.ObservedAt != "2026-09-22T08:00:00Z" {
		t.Errorf("controller = %+v", c)
	}
	// Reading it adds no write: the console offers no route that would update
	// the controller, and must not grow one by accident.
	for _, target := range []string{"/api/portal/operator/controller", "/api/portal/operator/controller/update"} {
		if rec := e.do(http.MethodPost, target, nil); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404 — the portal never updates the controller", target, rec.Code)
		}
	}
}

// An operator that predates the field reports nothing, and the answer says
// nothing rather than inventing blanks: the key is absent, so a client can
// tell "not reported" from "reported empty" and word it differently.
func TestOperatorGetOmitsAnUnreportedController(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"an older operator":         nil,
		"an empty controller block": {"controller": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newOpEnv(t)
			e.putCRWithStatus("", extra)

			rec := e.do(http.MethodGet, "/api/portal/operator", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET operator = %d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), `"controller"`) {
				t.Errorf("an unreported controller must be omitted: %s", rec.Body)
			}
			var state struct {
				Operator operator.OperatorInfo `json:"operator"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
			// Everything else about the console is unaffected.
			if !state.Operator.Present || state.Operator.Controller != nil {
				t.Errorf("operator = %+v", state.Operator)
			}
		})
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

// apply-update names the update it applies — the one the caller was shown —
// so that a channel that moved, or a release that landed, while the caller
// decided is refused instead of rolled out. Without the name there is nothing
// to hold the operator's answer against, so it is refused too.
func TestApplyUpdateNamesTheVersionItApplies(t *testing.T) {
	// No body at all: refused, and nothing is written.
	e := newOpEnv(t)
	e.putCR("1.5.0")
	for _, body := range []any{nil, map[string]any{}, map[string]any{"version": "  "}} {
		rec := e.do(http.MethodPost, "/api/portal/operator/apply-update", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"version"`) {
			t.Errorf("apply-update with %v = %d %s, want 400 naming version", body, rec.Code, rec.Body)
		}
	}
	if v, _ := e.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any)["version"].(string); v != "1.4.0" {
		t.Errorf("a refused apply must write nothing: spec.version = %q", v)
	}

	// The version the caller decided on, and it still matches.
	e2 := newOpEnv(t)
	e2.putCR("1.5.0")
	rec := e2.do(http.MethodPost, "/api/portal/operator/apply-update", map[string]any{"version": "1.5.0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply-update with the matching version = %d %s", rec.Code, rec.Body)
	}
	if body := e2.decodeBody(rec); body["version"] != "1.5.0" || body["generation"] != float64(2) {
		t.Fatalf("apply-update answered %v", body)
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
	if rec = e4.do(http.MethodPost, "/api/portal/operator/apply-update", map[string]any{"version": "1.5.0"}); rec.Code != http.StatusBadRequest {
		t.Errorf("no update available = %d %s, want 400", rec.Code, rec.Body)
	}

	// A body that is sent must still be a well-formed one.
	e5 := newOpEnv(t)
	e5.putCR("1.5.0")
	if rec = e5.do(http.MethodPost, "/api/portal/operator/apply-update", `{"version":"1.5.0","channel":"edge"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field = %d, want 400", rec.Code)
	}
}

// The admin console runs on its own stack — portal-api, the portal, sign-in,
// the registry database, the catalog manager. Scaling one of them to zero
// from the console would leave nothing to scale it back up from, so the
// platform keeps one replica; the console reads which they are.
func TestTheAdminStackKeepsAReplica(t *testing.T) {
	e := newOpEnv(t, "postgres", "keycloak")
	for _, name := range []string{"portal-api", "zaentrum-portal", "katalog-manager-api", "chino-api"} {
		e.putDeployment(name, 2, false)
	}
	e.putDeployment("operator-owned", 1, true)
	for _, name := range []string{"portal-api", "zaentrum-portal", "katalog-manager-api"} {
		rec := e.do(http.MethodPost, "/api/portal/operator/instances/"+name+"/scale", map[string]any{"replicas": 0})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "keeps at least one replica") {
			t.Errorf("scale %s to 0 = %d %s, want 400 saying why", name, rec.Code, rec.Body)
		}
		if got := e.kube.Object("deployments", name)["spec"].(map[string]any)["replicas"]; fmt.Sprint(got) != "2" {
			t.Errorf("a refused scale of %s wrote replicas = %v", name, got)
		}
		// Down to one, and up, as before.
		if rec := e.do(http.MethodPost, "/api/portal/operator/instances/"+name+"/scale", map[string]any{"replicas": 1}); rec.Code != http.StatusOK {
			t.Errorf("scale %s to 1 = %d %s", name, rec.Code, rec.Body)
		}
	}
	// Everything else still scales to zero.
	if rec := e.do(http.MethodPost, "/api/portal/operator/instances/chino-api/scale", map[string]any{"replicas": 0}); rec.Code != http.StatusOK {
		t.Errorf("scale chino-api to 0 = %d %s", rec.Code, rec.Body)
	}
	// The console reads which workloads are the admin stack, and how each
	// replaces its pods.
	rec := e.do(http.MethodGet, "/api/portal/operator", nil)
	var state struct {
		Instances []operator.Instance `json:"instances"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &state)
	for _, in := range state.Instances {
		want := in.Name == "portal-api" || in.Name == "zaentrum-portal" || in.Name == "katalog-manager-api"
		if in.AdminStack != want {
			t.Errorf("%s adminStack = %v", in.Name, in.AdminStack)
		}
	}
}

// A restart of the admin stack is safe when the Deployment rolls — the
// console answers from the old pod until the new one is ready — and refused
// when it recreates: the old pod would stop first, and a new one that never
// comes up would leave nothing to answer.
func TestTheAdminStackRestartsOnlyWhenItRolls(t *testing.T) {
	e := newOpEnv(t)
	e.putDeploymentWith("portal-api", 1, false, "RollingUpdate")
	e.putDeploymentWith("zaentrum-portal", 1, false, "")
	e.putDeploymentWith("katalog-manager-api", 1, false, "Recreate")
	e.putDeploymentWith("transcoder", 1, false, "Recreate")
	for name, want := range map[string]int{
		"portal-api": http.StatusOK, "zaentrum-portal": http.StatusOK,
		"katalog-manager-api": http.StatusBadRequest,
		"transcoder":          http.StatusOK, // not the admin stack: its own downtime is its own
		"nosuch-api":          http.StatusNotFound,
	} {
		rec := e.do(http.MethodPost, "/api/portal/operator/instances/"+name+"/restart", nil)
		if rec.Code != want {
			t.Errorf("restart %s = %d %s, want %d", name, rec.Code, rec.Body, want)
		}
	}
	if rec := e.do(http.MethodPost, "/api/portal/operator/instances/katalog-manager-api/restart", nil); !strings.Contains(rec.Body.String(), "recreates its pods") {
		t.Errorf("the refusal says why: %s", rec.Body)
	}
	if got := e.kube.Object("deployments", "katalog-manager-api")["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)[k8s.RestartedAtAnnotation]; got != "2026-09-21T08:00:00Z" {
		t.Errorf("a refused restart wrote a stamp: %v", got)
	}
}
