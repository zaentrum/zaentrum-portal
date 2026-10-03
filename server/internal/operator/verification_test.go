package operator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
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

// ─── asking for a run ───────────────────────────────────────────────────────

// putVerifiable seeds the operator's resource with what verification reads.
func putVerifiable(fake *k8sfake.Server, spec, annotations, record map[string]any) {
	if spec == nil {
		spec = map[string]any{}
	}
	spec["version"] = "1.4.0"
	md := map[string]any{"name": "zaentrum"}
	if annotations != nil {
		md["annotations"] = annotations
	}
	fake.Put(zaentrums, map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum", "metadata": md, "spec": spec})
	status := map[string]any{"phase": "Ready", "currentVersion": "1.4.0", "observedGeneration": 1}
	if record != nil {
		status["verification"] = record
	}
	fake.SetStatus(zaentrums, "zaentrum", status)
}

func annotation(fake *k8sfake.Server) string {
	md, _ := fake.Object(zaentrums, "zaentrum")["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	s, _ := ann[AnnotationVerifyRequest].(string)
	return s
}

func patches(fake *k8sfake.Server) []k8sfake.Call {
	var out []k8sfake.Call
	for _, c := range fake.Calls() {
		if c.Method == http.MethodPatch {
			out = append(out, c)
		}
	}
	return out
}

// beforePatch runs hook before each PATCH the service sends — the moment
// between its read and its write, where somebody else's write lands.
type beforePatch struct {
	base http.RoundTripper
	n    int
	hook func(n int)
}

func (b *beforePatch) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPatch {
		b.n++
		b.hook(b.n)
	}
	return b.base.RoundTrip(r)
}

func racedService(t *testing.T, fake *k8sfake.Server, hook func(n int)) *Service {
	t.Helper()
	hc := &http.Client{Transport: &beforePatch{base: fake.Server.Client().Transport, hook: hook}}
	cfg := config.Config{OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", OperatorPlural: zaentrums}
	s := New(k8s.NewAt(fake.URL, "zaentrum", "test-token", hc), cfg)
	s.token = func() string { return "mine" }
	return s
}

func TestRequestVerificationWritesTheAnnotationAndNothingElse(t *testing.T) {
	s, fake := rolloutService(t)
	putVerifiable(fake, nil, nil, failedRun())
	before := fake.Object(zaentrums, "zaentrum")
	s.token = func() string { return "tok-2" }

	out, err := s.RequestVerification(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Request != "tok-2" || out.Joined {
		t.Fatalf("request = %+v", out)
	}
	if got := annotation(fake); got != "tok-2" {
		t.Errorf("annotation = %q", got)
	}
	after := fake.Object(zaentrums, "zaentrum")
	if asJSON(t, after["spec"]) != asJSON(t, before["spec"]) || fake.Generation(zaentrums, "zaentrum") != 1 {
		t.Errorf("a request changes no spec and makes no generation: %v", after["spec"])
	}
	// The write is a merge patch on metadata alone, conditional on the
	// version the decision was made on.
	ps := patches(fake)
	if len(ps) != 1 {
		t.Fatalf("patches = %+v", ps)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal([]byte(ps[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["metadata"] == nil || ps[0].ContentType != "application/merge-patch+json" {
		t.Errorf("patch = %s (%s), want metadata only, as a merge patch", ps[0].Body, ps[0].ContentType)
	}
	if body["metadata"]["resourceVersion"] != before["metadata"].(map[string]any)["resourceVersion"] {
		t.Errorf("the patch must name the resourceVersion it was decided on: %s", ps[0].Body)
	}
}

// The default token is random: two requests in a row are two requests.
func TestRequestVerificationTokensAreFresh(t *testing.T) {
	s, fake := rolloutService(t)
	putVerifiable(fake, nil, nil, nil)
	first, err := s.RequestVerification(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The operator answers it.
	run := failedRun()
	run["request"] = first.Request
	fake.SetStatus(zaentrums, "zaentrum", map[string]any{"verification": run})
	second, err := s.RequestVerification(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Request) < 16 || second.Request == first.Request || second.Joined {
		t.Errorf("tokens %q then %+v, want two fresh ones", first.Request, second)
	}
}

// A request while a run is in progress waits for it: it is written, and the
// operator starts its run when the current one ends.
func TestRequestVerificationWhileARunIsInProgress(t *testing.T) {
	s, fake := rolloutService(t)
	running := map[string]any{"result": "Running", "trigger": "update", "request": "tok-1", "startedAt": "2026-10-03T08:00:00Z"}
	putVerifiable(fake, nil, map[string]any{AnnotationVerifyRequest: "tok-1"}, running)
	s.token = func() string { return "tok-2" }

	out, err := s.RequestVerification(context.Background())
	if err != nil || out.Request != "tok-2" || out.Joined {
		t.Fatalf("request = %+v, %v", out, err)
	}
	if got := annotation(fake); got != "tok-2" {
		t.Errorf("annotation = %q, want the new request waiting behind the run", got)
	}
}

// A request already waiting is joined, not replaced: replacing it would leave
// whoever asked first following a run that never comes.
func TestRequestVerificationJoinsAWaitingRequest(t *testing.T) {
	s, fake := rolloutService(t)
	putVerifiable(fake, nil, map[string]any{AnnotationVerifyRequest: "tok-2"}, failedRun())
	s.token = func() string { return "tok-3" }

	out, err := s.RequestVerification(context.Background())
	if err != nil || out.Request != "tok-2" || !out.Joined {
		t.Fatalf("request = %+v, %v — want to join tok-2", out, err)
	}
	if len(patches(fake)) != 0 || annotation(fake) != "tok-2" {
		t.Errorf("joining writes nothing: patches %v, annotation %q", patches(fake), annotation(fake))
	}
}

func TestRequestVerificationRefusesWhenSwitchedOff(t *testing.T) {
	s, fake := rolloutService(t)
	putVerifiable(fake, map[string]any{"verification": map[string]any{"enabled": false}}, nil, nil)

	_, err := s.RequestVerification(context.Background())
	if !errors.Is(err, ErrVerificationDisabled) || !strings.Contains(err.Error(), "spec.verification.enabled") {
		t.Fatalf("want ErrVerificationDisabled naming the field, got %v", err)
	}
	if len(patches(fake)) != 0 {
		t.Errorf("a refused request writes nothing: %v", patches(fake))
	}
	// Even a waiting request is not offered to join: nothing will run it.
	s2, fake2 := rolloutService(t)
	putVerifiable(fake2, map[string]any{"verification": map[string]any{"enabled": false}}, map[string]any{AnnotationVerifyRequest: "tok-2"}, nil)
	if _, err := s2.RequestVerification(context.Background()); !errors.Is(err, ErrVerificationDisabled) {
		t.Errorf("want ErrVerificationDisabled, got %v", err)
	}
}

func TestRequestVerificationWithoutAnOperator(t *testing.T) {
	s, _ := rolloutService(t)
	if _, err := s.RequestVerification(context.Background()); err == nil || !strings.Contains(err.Error(), "no operator instance to verify") {
		t.Errorf("no resource: %v", err)
	}
	if _, err := newSvc(t).RequestVerification(context.Background()); err == nil || !strings.Contains(err.Error(), "not running in a cluster") {
		t.Errorf("outside a cluster: %v", err)
	}
}

// The operator writes its status between the read and the write: the write is
// refused, and the decision is made again from what is there now.
func TestRequestVerificationRedecidesAfterALostRace(t *testing.T) {
	fake := k8sfake.New(t)
	putVerifiable(fake, nil, nil, failedRun())
	s := racedService(t, fake, func(n int) {
		if n == 1 {
			fake.SetStatus(zaentrums, "zaentrum", map[string]any{"phase": "Ready", "verification": failedRun()})
		}
	})

	out, err := s.RequestVerification(context.Background())
	if err != nil || out.Request != "mine" || out.Joined {
		t.Fatalf("request = %+v, %v", out, err)
	}
	if n := len(patches(fake)); n != 2 || annotation(fake) != "mine" {
		t.Errorf("patches = %d, annotation %q — want the stale write refused and the second one landed", n, annotation(fake))
	}
}

// Somebody else asks between the read and the write: their request is waiting
// now, and this one joins it rather than writing over it.
func TestRequestVerificationJoinsARequestWrittenMeanwhile(t *testing.T) {
	fake := k8sfake.New(t)
	putVerifiable(fake, nil, nil, failedRun())
	s := racedService(t, fake, func(n int) {
		if n == 1 {
			obj := fake.Object(zaentrums, "zaentrum")
			obj["metadata"].(map[string]any)["annotations"] = map[string]any{AnnotationVerifyRequest: "theirs"}
			fake.Put(zaentrums, obj)
		}
	})

	out, err := s.RequestVerification(context.Background())
	if err != nil || out.Request != "theirs" || !out.Joined {
		t.Fatalf("request = %+v, %v — want to join theirs", out, err)
	}
	if annotation(fake) != "theirs" || len(patches(fake)) != 1 {
		t.Errorf("annotation %q after %d patches — theirs must stand", annotation(fake), len(patches(fake)))
	}
}

func TestRequestVerificationGivesUpOnAResourceThatKeepsChanging(t *testing.T) {
	s, fake := rolloutService(t)
	putVerifiable(fake, nil, nil, nil)
	fake.Fail = func(c k8sfake.Call) (int, string, bool) {
		return http.StatusConflict, "the object has been modified", c.Method == http.MethodPatch
	}
	_, err := s.RequestVerification(context.Background())
	if !k8s.IsConflict(err) || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("want a conflict that says to try again, got %v", err)
	}
	if n := len(patches(fake)); n != verifyAttempts {
		t.Errorf("patches = %d, want %d attempts", n, verifyAttempts)
	}
}

// Any other refusal is final: retrying a write the Role forbids changes nothing.
func TestRequestVerificationReturnsOtherRefusalsAtOnce(t *testing.T) {
	s, fake := rolloutService(t)
	putVerifiable(fake, nil, nil, nil)
	fake.Fail = func(c k8sfake.Call) (int, string, bool) {
		return http.StatusForbidden, "zaentrums is forbidden", c.Method == http.MethodPatch
	}
	if _, err := s.RequestVerification(context.Background()); !k8s.IsForbidden(err) {
		t.Fatalf("want the Forbidden, got %v", err)
	}
	if n := len(patches(fake)); n != 1 {
		t.Errorf("patches = %d, want 1", n)
	}
}
