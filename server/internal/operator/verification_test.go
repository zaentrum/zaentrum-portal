package operator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// What a run looks like in the operator's status: a request-triggered run
// that found one failing check and one warning.
func failedRun() map[string]any {
	return map[string]any{
		"result": "Failed", "trigger": "request", "request": "tok-1",
		"fingerprint": "sha256:feed", "version": "v0.5.0",
		"startedAt": "2026-10-03T08:00:00Z", "finishedAt": "2026-10-03T08:01:30Z",
		"job": "zaentrum-verify-7f3k2", "passed": 12, "failed": 1, "warned": 1, "skipped": 0,
		"checks": []any{
			map[string]any{"name": "portal-api ready", "status": "ok", "detail": ""},
			map[string]any{"name": "chino-api health", "status": "fail", "detail": "503 from /healthz"},
			map[string]any{"name": "issuer latency", "status": "warn", "detail": "1.8 s, over 1 s"},
		},
		"message": "1 of 14 checks failed",
	}
}

// cr builds the operator's resource from the three places verification lives:
// the spec, the annotations, and the status.
func cr(t *testing.T, spec, annotations, status map[string]any) zaentrumCR {
	t.Helper()
	obj := map[string]any{"metadata": map[string]any{"name": "zaentrum", "annotations": annotations}, "spec": spec, "status": status}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var it zaentrumCR
	if err := json.Unmarshal(raw, &it); err != nil {
		t.Fatalf("the resource must decode whatever its verification says: %v", err)
	}
	return it
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Every operator older than verification writes none of it, and a client has
// to be able to say "never" from what it gets — not from a row of zeros.
func TestVerificationOfAnOperatorThatReportsNothing(t *testing.T) {
	for name, status := range map[string]map[string]any{
		"no record at all":            {"phase": "Ready"},
		"a null record":               {"phase": "Ready", "verification": nil},
		"a record with nothing in it": {"phase": "Ready", "verification": map[string]any{}},
		// A record without a result is not a run anyone can read.
		"a record without a result": {"phase": "Ready", "verification": map[string]any{"result": "  ", "passed": 3}},
	} {
		t.Run(name, func(t *testing.T) {
			v := verificationOf(cr(t, nil, nil, status))
			if got := asJSON(t, v); got != `{"enabled":true,"result":null}` {
				t.Errorf("verification = %s, want exactly enabled and a null result", got)
			}
		})
	}
}

func TestVerificationOfARun(t *testing.T) {
	v := verificationOf(cr(t, nil, nil, map[string]any{
		"verification": failedRun(),
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "Reconciled", "lastTransitionTime": "2026-10-01T08:00:00Z"},
			map[string]any{"type": "Verified", "status": "False", "reason": "ChecksFailed", "lastTransitionTime": "2026-10-03T08:01:30Z"},
		},
	}))
	if v.Result == nil || *v.Result != ResultFailed || v.VerificationRun == nil {
		t.Fatalf("verification = %s", asJSON(t, v))
	}
	run := *v.VerificationRun
	if run.Trigger != "request" || run.Request != "tok-1" || run.Fingerprint != "sha256:feed" || run.Version != "v0.5.0" ||
		run.StartedAt != "2026-10-03T08:00:00Z" || run.FinishedAt != "2026-10-03T08:01:30Z" || run.Job != "zaentrum-verify-7f3k2" ||
		run.Passed != 12 || run.Failed != 1 || run.Warned != 1 || run.Skipped != 0 || run.Message != "1 of 14 checks failed" {
		t.Errorf("run = %+v", run)
	}
	if len(run.Checks) != 3 || run.Checks[1] != (VerificationCheck{Name: "chino-api health", Status: "fail", Detail: "503 from /healthz"}) {
		t.Errorf("checks = %+v", run.Checks)
	}
	// The Verified condition, not the first condition there is.
	if v.Condition == nil || *v.Condition != (VerifiedCondition{Status: "False", Reason: "ChecksFailed", LastTransitionTime: "2026-10-03T08:01:30Z"}) {
		t.Errorf("condition = %+v", v.Condition)
	}
	// A count of zero is said, not left out: 0 skipped is part of the answer.
	raw := asJSON(t, v)
	for _, field := range []string{`"enabled":true`, `"result":"Failed"`, `"skipped":0`, `"checks":[`, `"condition":{`} {
		if !strings.Contains(raw, field) {
			t.Errorf("the document must carry %s: %s", field, raw)
		}
	}
	if strings.Contains(raw, `"pendingRequest"`) || strings.Contains(raw, `"note"`) {
		t.Errorf("nothing waits and everything was read: %s", raw)
	}
}

// A run in progress has no checks yet, and says so with an empty list rather
// than a null a client would have to special-case.
func TestVerificationOfARunInProgress(t *testing.T) {
	v := verificationOf(cr(t, nil, nil, map[string]any{"verification": map[string]any{
		"result": "Running", "trigger": "update", "version": "v0.5.0", "startedAt": "2026-10-03T08:00:00Z",
	}}))
	if v.Result == nil || *v.Result != ResultRunning || v.Checks == nil || len(v.Checks) != 0 {
		t.Fatalf("verification = %s", asJSON(t, v))
	}
	if !strings.Contains(asJSON(t, v), `"checks":[]`) {
		t.Errorf("checks must be an empty list: %s", asJSON(t, v))
	}
	if v.Condition != nil {
		t.Errorf("no Verified condition was reported: %+v", v.Condition)
	}
}

func TestVerificationEnabled(t *testing.T) {
	for name, c := range map[string]struct {
		spec map[string]any
		want bool
	}{
		"a spec that says nothing is on": {nil, true},
		"an empty verification block":    {map[string]any{"verification": map[string]any{}}, true},
		"switched on":                    {map[string]any{"verification": map[string]any{"enabled": true}}, true},
		"switched off":                   {map[string]any{"verification": map[string]any{"enabled": false}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := verificationOf(cr(t, c.spec, nil, nil)).Enabled; got != c.want {
				t.Errorf("enabled = %v, want %v", got, c.want)
			}
		})
	}
	// Switched off, the last record is still the truth about the last run.
	v := verificationOf(cr(t, map[string]any{"verification": map[string]any{"enabled": false}}, nil, map[string]any{"verification": failedRun()}))
	if v.Enabled || v.Result == nil || *v.Result != ResultFailed {
		t.Errorf("verification = %s", asJSON(t, v))
	}
}

// A request waits while the annotation names one the record does not answer —
// the operator's own comparison.
func TestPendingRequest(t *testing.T) {
	run := func(request string) map[string]any {
		r := failedRun()
		r["request"] = request
		return map[string]any{"verification": r}
	}
	for name, c := range map[string]struct {
		annotations map[string]any
		status      map[string]any
		want        string
	}{
		"nothing asked":                    {nil, run("tok-1"), ""},
		"a blank annotation asks nothing":  {map[string]any{AnnotationVerifyRequest: "  "}, run("tok-1"), ""},
		"the run answered it":              {map[string]any{AnnotationVerifyRequest: "tok-1"}, run("tok-1"), ""},
		"asked after the last run":         {map[string]any{AnnotationVerifyRequest: "tok-2"}, run("tok-1"), "tok-2"},
		"asked, and nothing has run yet":   {map[string]any{AnnotationVerifyRequest: "tok-2"}, nil, "tok-2"},
		"asked, and an update ran instead": {map[string]any{AnnotationVerifyRequest: "tok-2"}, run(""), "tok-2"},
		"other annotations are not asking": {map[string]any{"zaentrum.io/keep-values": "true"}, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := verificationOf(cr(t, nil, c.annotations, c.status)).PendingRequest; got != c.want {
				t.Errorf("pendingRequest = %q, want %q", got, c.want)
			}
		})
	}
}

// A record this portal cannot read costs the console that record — not the
// operator card, and not the scale, restart and update beside it.
func TestAnUnreadableRecordIsANote(t *testing.T) {
	s, fake := rolloutService(t)
	putCR(fake, "1.4.0", "stable", "1.5.0")
	obj := fake.Object(zaentrums, "zaentrum")
	obj["metadata"].(map[string]any)["annotations"] = map[string]any{AnnotationVerifyRequest: "tok-2"}
	fake.Put(zaentrums, obj)
	fake.SetStatus(zaentrums, "zaentrum", map[string]any{
		"phase": "Ready", "currentVersion": "1.4.0", "availableUpdate": "1.5.0",
		"verification": map[string]any{"result": "Passed", "passed": "fourteen"},
	})

	info, err := s.OperatorInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Present || info.AvailableUpdate != "1.5.0" {
		t.Fatalf("the rest of the console must survive: %+v", info)
	}
	v := info.Verification
	if v == nil || v.Note == "" || v.Result != nil || v.VerificationRun != nil {
		t.Fatalf("verification = %s", asJSON(t, v))
	}
	// Whether tok-2 was answered is in the record that could not be read.
	if v.PendingRequest != "" {
		t.Errorf("an unreadable record answers nothing about a request: %q", v.PendingRequest)
	}
}
