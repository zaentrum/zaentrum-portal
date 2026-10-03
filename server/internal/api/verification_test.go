package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// The platform verifies itself after every update, and the console shows how
// that went. These tests pin the wire contract the console and the CLI read:
// the verification part of GET /operator.

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
