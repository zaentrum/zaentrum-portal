package api

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// The platform verifies itself after every update; the console shows how that
// went and asks for a run between updates. These tests pin the wire contract
// the console and the CLI read: the verification part of GET /operator, and
// POST /operator/verify.

// putVerifiable seeds the operator's resource with the three places
// verification lives: the spec, the annotations and the status.
func (e *opEnv) putVerifiable(spec, annotations, record map[string]any) {
	if spec == nil {
		spec = map[string]any{}
	}
	spec["version"], spec["channel"] = "1.4.0", "stable"
	md := map[string]any{"name": "zaentrum"}
	if annotations != nil {
		md["annotations"] = annotations
	}
	e.kube.Put("zaentrums", map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum", "metadata": md, "spec": spec})
	status := map[string]any{"phase": "Ready", "currentVersion": "1.4.0", "observedGeneration": 1}
	if record != nil {
		status["verification"] = record
	}
	e.kube.SetStatus("zaentrums", "zaentrum", status)
}

// verification is the document's verification part, raw.
func (e *opEnv) verification() map[string]any {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/portal/operator", nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET operator = %d %s", rec.Code, rec.Body)
	}
	op, _ := e.decodeBody(rec)["operator"].(map[string]any)
	v, _ := op["verification"].(map[string]any)
	return v
}

func (e *opEnv) requestAnnotation() string {
	md, _ := e.kube.Object("zaentrums", "zaentrum")["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	s, _ := ann[operator.AnnotationVerifyRequest].(string)
	return s
}

func (e *opEnv) patches() []k8sfake.Call {
	var out []k8sfake.Call
	for _, c := range e.kube.Calls() {
		if c.Method == http.MethodPatch {
			out = append(out, c)
		}
	}
	return out
}

func passedRun(request string) map[string]any {
	return map[string]any{
		"result": "Passed", "trigger": "update", "request": request, "fingerprint": "sha256:feed", "version": "1.4.0",
		"startedAt": "2026-10-03T08:00:00Z", "finishedAt": "2026-10-03T08:01:30Z", "job": "zaentrum-verify-7f3k2",
		"passed": 13, "failed": 0, "warned": 1, "skipped": 0, "message": "all checks passed",
		"checks": []any{
			map[string]any{"name": "portal-api ready", "status": "ok", "detail": ""},
			map[string]any{"name": "issuer latency", "status": "warn", "detail": "1.8 s, over 1 s"},
		},
	}
}

func TestOperatorGetCarriesTheVerification(t *testing.T) {
	e := newOpEnv(t)
	e.putVerifiable(nil, map[string]any{operator.AnnotationVerifyRequest: "tok-2"}, passedRun("tok-1"))
	e.kube.SetStatus("zaentrums", "zaentrum", map[string]any{
		"phase": "Ready", "verification": passedRun("tok-1"),
		"conditions": []any{map[string]any{"type": "Verified", "status": "True", "reason": "ChecksPassed", "lastTransitionTime": "2026-10-03T08:01:30Z"}},
	})

	v := e.verification()
	// Every field of the record, under the operator's own names.
	want := map[string]any{
		"enabled": true, "result": "Passed", "trigger": "update", "request": "tok-1", "fingerprint": "sha256:feed",
		"version": "1.4.0", "startedAt": "2026-10-03T08:00:00Z", "finishedAt": "2026-10-03T08:01:30Z",
		"job": "zaentrum-verify-7f3k2", "passed": float64(13), "failed": float64(0), "warned": float64(1),
		"skipped": float64(0), "message": "all checks passed", "pendingRequest": "tok-2",
	}
	for k, w := range want {
		if v[k] != w {
			t.Errorf("verification.%s = %#v, want %#v", k, v[k], w)
		}
	}
	checks, _ := v["checks"].([]any)
	if len(checks) != 2 || checks[1].(map[string]any)["detail"] != "1.8 s, over 1 s" {
		t.Errorf("checks = %v", v["checks"])
	}
	cond, _ := v["condition"].(map[string]any)
	if cond["status"] != "True" || cond["reason"] != "ChecksPassed" || cond["lastTransitionTime"] != "2026-10-03T08:01:30Z" {
		t.Errorf("condition = %v", v["condition"])
	}
}

// An operator older than verification reports none, and the document says so
// in the one way a client can branch on — result null — without breaking.
func TestOperatorGetSaysNeverForAnOlderOperator(t *testing.T) {
	e := newOpEnv(t)
	e.putCR("1.5.0")

	rec := e.do(http.MethodGet, "/api/portal/operator", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET operator = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"verification":{"enabled":true,"result":null}`) {
		t.Errorf("want verification {enabled, result: null}: %s", rec.Body)
	}
	var state struct {
		Operator operator.OperatorInfo `json:"operator"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Operator.Present || state.Operator.AvailableUpdate != "1.5.0" {
		t.Errorf("the rest of the document is unaffected: %+v", state.Operator)
	}

	// Switched off reads as off, with the same null for a run that never was.
	e2 := newOpEnv(t)
	e2.putVerifiable(map[string]any{"verification": map[string]any{"enabled": false}}, nil, nil)
	if v := e2.verification(); v["enabled"] != false || v["result"] != nil || len(v) != 2 {
		t.Errorf("verification = %v", v)
	}
}

// No operator, nothing to verify and nobody to ask: the part is absent.
func TestOperatorGetWithoutAnOperatorHasNoVerification(t *testing.T) {
	e := newOpEnv(t)
	rec := e.do(http.MethodGet, "/api/portal/operator", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"verification"`) {
		t.Errorf("GET operator = %d %s", rec.Code, rec.Body)
	}
}

func TestVerifyWritesTheRequestAndNothingElse(t *testing.T) {
	e := newOpEnv(t)
	e.putVerifiable(nil, nil, passedRun(""))
	before := e.kube.Object("zaentrums", "zaentrum")

	rec := e.do(http.MethodPost, "/api/portal/operator/verify", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body)
	}
	body := e.decodeBody(rec)
	token, _ := body["request"].(string)
	if token == "" || len(body) != 1 {
		t.Fatalf("verify answered %v, want {request}", body)
	}
	if got := e.requestAnnotation(); got != token {
		t.Errorf("annotation = %q, answered %q", got, token)
	}
	after := e.kube.Object("zaentrums", "zaentrum")
	if mustJSON(t, after["spec"]) != mustJSON(t, before["spec"]) || e.kube.Generation("zaentrums", "zaentrum") != 1 {
		t.Errorf("a request changes no spec and makes no generation: %v", after["spec"])
	}
	ps := e.patches()
	if len(ps) != 1 {
		t.Fatalf("patches = %v", ps)
	}
	var patch map[string]any
	if err := json.Unmarshal([]byte(ps[0].Body), &patch); err != nil || len(patch) != 1 || patch["metadata"] == nil {
		t.Errorf("patch = %s, want metadata only", ps[0].Body)
	}
	// Until the operator starts its run, the document says it is waiting.
	if v := e.verification(); v["pendingRequest"] != token || v["result"] != "Passed" {
		t.Errorf("verification = %v", v)
	}
}

func TestVerifyRefusesWhenSwitchedOff(t *testing.T) {
	e := newOpEnv(t)
	e.putVerifiable(map[string]any{"verification": map[string]any{"enabled": false}}, nil, nil)

	rec := e.do(http.MethodPost, "/api/portal/operator/verify", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "spec.verification.enabled is false") {
		t.Fatalf("verify = %d %s, want 409 naming the switch", rec.Code, rec.Body)
	}
	if len(e.patches()) != 0 || e.requestAnnotation() != "" {
		t.Errorf("a refused request writes nothing: %v", e.patches())
	}
}

// A second request while a run is in progress is not refused: it waits, and
// the operator runs it once the current run ends.
func TestVerifyWhileARunIsInProgressWaits(t *testing.T) {
	e := newOpEnv(t)
	running := map[string]any{"result": "Running", "trigger": "request", "request": "tok-1", "startedAt": "2026-10-03T08:00:00Z"}
	e.putVerifiable(nil, map[string]any{operator.AnnotationVerifyRequest: "tok-1"}, running)

	rec := e.do(http.MethodPost, "/api/portal/operator/verify", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body)
	}
	token, _ := e.decodeBody(rec)["request"].(string)
	if token == "" || token == "tok-1" {
		t.Fatalf("want a new request, got %q", token)
	}
	if v := e.verification(); v["result"] != "Running" || v["request"] != "tok-1" || v["pendingRequest"] != token {
		t.Errorf("verification = %v, want tok-1 running and the new request waiting", v)
	}
}

// A request already waiting is joined: both callers follow the one run.
func TestVerifyJoinsAWaitingRequest(t *testing.T) {
	e := newOpEnv(t)
	e.putVerifiable(nil, map[string]any{operator.AnnotationVerifyRequest: "tok-2"}, passedRun("tok-1"))

	rec := e.do(http.MethodPost, "/api/portal/operator/verify", nil)
	if rec.Code != http.StatusAccepted || e.decodeBody(rec)["request"] != "tok-2" {
		t.Fatalf("verify = %d %s, want tok-2", rec.Code, rec.Body)
	}
	if len(e.patches()) != 0 {
		t.Errorf("joining writes nothing: %v", e.patches())
	}
}

func TestVerifyIsAdminOnly(t *testing.T) {
	e := newOpEnvAs(t, "zaentrum-viewer")
	e.putVerifiable(nil, nil, nil)
	rec := e.do(http.MethodPost, "/api/portal/operator/verify", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("verify by a non-admin = %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := e.do(http.MethodGet, "/api/portal/operator", nil); rec.Code != http.StatusForbidden {
		t.Errorf("the document is admin-only too: %d", rec.Code)
	}
	if len(e.patches()) != 0 {
		t.Errorf("a refused caller writes nothing: %v", e.patches())
	}

	// No bearer at all is 401, before any of it.
	closed, _ := auth.NewJWTVerifier(t.Context(), "http://127.0.0.1:1/realms/none", "", "zaentrum-admin", false, false)
	r := chi.NewRouter()
	(&API{addons: newFakeStore(), cfg: config.Config{AdminRole: "zaentrum-admin"}}).Register(r, auth.NewMiddleware(closed, "zaentrum-admin", "zaentrum-addon"))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/portal/operator/verify", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("verify without a bearer = %d", rec.Code)
	}
}

func TestVerifyTakesNoBody(t *testing.T) {
	e := newOpEnv(t)
	e.putVerifiable(nil, nil, nil)
	if rec := e.do(http.MethodPost, "/api/portal/operator/verify", `{"force":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field = %d %s, want 400", rec.Code, rec.Body)
	}
	if len(e.patches()) != 0 {
		t.Errorf("a refused body writes nothing: %v", e.patches())
	}
	if rec := e.do(http.MethodPost, "/api/portal/operator/verify", `{}`); rec.Code != http.StatusAccepted {
		t.Errorf("an empty object = %d %s, want 202", rec.Code, rec.Body)
	}
}

// Each refusal says what kind it is, so a client can tell "ask again" from
// "fix the platform" from "there is nothing here to verify".
func TestVerifyRefusalsByKind(t *testing.T) {
	forbidden := newOpEnv(t)
	forbidden.putVerifiable(nil, nil, nil)
	forbidden.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
		return http.StatusForbidden, "zaentrums is forbidden", c.Method == http.MethodPatch
	}
	if rec := forbidden.do(http.MethodPost, "/api/portal/operator/verify", nil); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "portal-api Role") {
		t.Errorf("a Role without patch = %d %s, want 503 naming the Role", rec.Code, rec.Body)
	}

	busy := newOpEnv(t)
	busy.putVerifiable(nil, nil, nil)
	busy.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
		return http.StatusConflict, "the object has been modified", c.Method == http.MethodPatch
	}
	if rec := busy.do(http.MethodPost, "/api/portal/operator/verify", nil); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "try again") {
		t.Errorf("a resource that keeps changing = %d %s, want 409", rec.Code, rec.Body)
	}

	none := newOpEnv(t)
	if rec := none.do(http.MethodPost, "/api/portal/operator/verify", nil); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "no operator instance to verify") {
		t.Errorf("no operator = %d %s, want 400", rec.Code, rec.Body)
	}

	// Outside a cluster the console's writes are unavailable, this one too.
	client, err := k8s.New()
	if err != nil {
		t.Fatal(err)
	}
	a := &API{addons: newFakeStore(), cfg: config.Config{AdminRole: "zaentrum-admin"}, op: operator.New(client, config.Config{})}
	jwt, _ := auth.NewJWTVerifier(t.Context(), "", "", "zaentrum-admin", false, true)
	r := chi.NewRouter()
	a.Register(r, auth.NewMiddleware(jwt, "zaentrum-admin", "zaentrum-addon"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/portal/operator/verify", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("outside a cluster = %d %s, want 503", rec.Code, rec.Body)
	}
}

// The request reaches the cluster as portal-api's service account, so the log
// line is the only record of the person behind it.
func TestVerifyLogsWhoAsked(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	e := newOpEnv(t)
	e.putVerifiable(nil, nil, nil)
	rec := e.do(http.MethodPost, "/api/portal/operator/verify", nil)
	token, _ := e.decodeBody(rec)["request"].(string)
	line := buf.String()
	if !strings.Contains(line, "anonymous asked for a verification") || !strings.Contains(line, token) || strings.Contains(line, "already waiting") {
		t.Errorf("log = %q, want who asked and request %s", line, token)
	}

	buf.Reset()
	e.do(http.MethodPost, "/api/portal/operator/verify", nil)
	if line := buf.String(); !strings.Contains(line, "anonymous asked") || !strings.Contains(line, token+" was already waiting") {
		t.Errorf("log = %q, want the join said", line)
	}
}

func TestRequester(t *testing.T) {
	req := func(p *auth.Principal) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if p != nil {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		return r
	}
	for want, r := range map[string]*http.Request{
		"admin":             req(&auth.Principal{Subject: "f1d2", Username: "admin"}),
		"f1d2":              req(&auth.Principal{Subject: "f1d2"}),
		"an unknown caller": req(nil),
	} {
		if got := requester(r); got != want {
			t.Errorf("requester = %q, want %q", got, want)
		}
	}
	if got := requester(req(&auth.Principal{})); got != "an unknown caller" {
		t.Errorf("a principal without a name = %q", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
