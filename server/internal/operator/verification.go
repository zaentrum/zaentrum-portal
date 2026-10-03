package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// The platform verifies itself. After every rollout the operator runs the
// platform's checks as a Job and records how it went in status.verification,
// with a Verified condition beside it; between rollouts anyone may ask for a
// run by giving the zaentrum.io/verify-request annotation a new value.
//
// The portal reads that record and writes that request, and does nothing
// else: it runs no check, judges no result and never writes a status. What it
// adds is the reading — "never" told apart from a run, and a request told
// apart from the run that answers it.

const (
	// AnnotationVerifyRequest asks the operator for a run. A value the
	// operator has not answered — one that is not status.verification.request
	// — is a request waiting, and the operator starts a run for it once the
	// run in progress, if there is one, ends.
	AnnotationVerifyRequest = "zaentrum.io/verify-request"
	// ConditionVerified is the condition the operator keeps beside the record.
	ConditionVerified = "Verified"
)

// Results a run reports, in the operator's words. Any other value is passed
// through as written: a result this portal has not heard of is still the
// operator's answer.
const (
	ResultPassed  = "Passed"
	ResultFailed  = "Failed"
	ResultRunning = "Running"
	ResultSkipped = "Skipped"
	ResultError   = "Error"
)

// verifyAttempts bounds the read-decide-write loop of a request. Each lost
// race with another write to the resource — usually the operator's own status
// — is decided again from a fresh read.
const verifyAttempts = 4

// ErrVerificationDisabled: spec.verification.enabled is false. The operator
// runs no checks, so a request would wait for a run that never comes; the API
// answers it 409 instead of accepting it.
var ErrVerificationDisabled = errors.New("verification is disabled")

// Verification is the record as the console and the CLI read it.
//
// Result is the field a client branches on, and it is null when the operator
// has reported no run — "never", which is also what every operator older than
// verification reports. The run's own fields are then absent from the JSON
// rather than present and zero: a count of 0 failed must not stand in for
// checks that were never run.
type Verification struct {
	// Enabled is spec.verification.enabled: whether the operator verifies
	// after a rollout and answers a request. A spec that says nothing gets the
	// operator's default, which is on.
	Enabled bool `json:"enabled"`
	// Result is Passed | Failed | Running | Skipped | Error, or null: never.
	Result *string `json:"result"`
	// The last run, or the one in progress. Embedded so that its fields sit
	// beside result as they do in the status; nil when there was none, and
	// then they are left out.
	*VerificationRun
	// Condition is the resource's Verified condition, when it has one.
	Condition *VerifiedCondition `json:"condition,omitempty"`
	// PendingRequest is a request no run has answered yet: the annotation's
	// value, when it is not the request the record carries. Omitted when no
	// request is waiting.
	PendingRequest string `json:"pendingRequest,omitempty"`
	// Note says why the record could not be read, when it could not.
	Note string `json:"note,omitempty"`
}

// VerificationRun is one run as status.verification records it, without its
// result, which Verification carries.
type VerificationRun struct {
	// Trigger is what started it: update (a rollout) or request.
	Trigger string `json:"trigger"`
	// Request is the token of the request the run answers. A client that
	// asked follows the document until this is its token and the result is no
	// longer Running.
	Request string `json:"request"`
	// Fingerprint identifies the platform state the run checked, Version the
	// platform version it checked.
	Fingerprint string `json:"fingerprint"`
	Version     string `json:"version"`
	// StartedAt and FinishedAt are RFC 3339; FinishedAt is empty while the run
	// is in progress.
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	// Job is the Job that ran the checks, which is where their logs are.
	Job string `json:"job"`
	// How the checks ended, counted, and each check.
	Passed  int                 `json:"passed"`
	Failed  int                 `json:"failed"`
	Warned  int                 `json:"warned"`
	Skipped int                 `json:"skipped"`
	Checks  []VerificationCheck `json:"checks"`
	// Message is the operator's one line about the run.
	Message string `json:"message"`
}

// VerificationCheck is one check of a run.
type VerificationCheck struct {
	Name string `json:"name"`
	// Status is ok | warn | fail | skip.
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// VerifiedCondition is the part of the Verified condition a reader needs.
type VerifiedCondition struct {
	Status             string `json:"status"` // True | False | Unknown
	Reason             string `json:"reason"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

// VerifyRequest is what asking for a run produced: the token the run that
// answers it will carry in status.verification.request.
type VerifyRequest struct {
	Request string `json:"request"`
	// Joined: a request was already waiting and this one joined it instead of
	// writing another, so one run answers both. Said in the log, not on the
	// wire — the caller follows the token either way.
	Joined bool `json:"-"`
}

// verificationRecord is status.verification as the operator writes it.
type verificationRecord struct {
	Result string `json:"result"`
	VerificationRun
}

// verificationOf reads what the resource says about verification. It answers
// for every resource, the older operator's included: no record is a null
// result, and a record that cannot be read is a note, never an error that
// would take the rest of the console with it.
func verificationOf(it zaentrumCR) *Verification {
	v := &Verification{
		Enabled:   it.Spec.Verification.Enabled == nil || *it.Spec.Verification.Enabled,
		Condition: verifiedCondition(it.Status.Conditions),
	}
	var rec verificationRecord
	if !isNull(it.Status.Verification) {
		if err := json.Unmarshal(it.Status.Verification, &rec); err != nil {
			// Whether the annotation was answered is in the record too, so
			// nothing is said about a request either.
			v.Note = "the operator's verification record cannot be read: " + err.Error()
			return v
		}
	}
	// A record without a result is not a run anyone can read; it says no
	// more than no record at all.
	if result := strings.TrimSpace(rec.Result); result != "" {
		run := rec.VerificationRun
		if run.Checks == nil {
			run.Checks = []VerificationCheck{}
		}
		v.Result, v.VerificationRun = &result, &run
	}
	v.PendingRequest = pendingRequest(it.Metadata.Annotations, v.answered())
	return v
}

// answered is the request the recorded run answers, "" when there is none.
func (v *Verification) answered() string {
	if v.VerificationRun == nil {
		return ""
	}
	return v.Request
}

// verifiedCondition is the Verified condition, nil when there is none.
func verifiedCondition(conds []condition) *VerifiedCondition {
	for _, c := range conds {
		if c.Type == ConditionVerified {
			return &VerifiedCondition{Status: c.Status, Reason: c.Reason, LastTransitionTime: c.LastTransitionTime}
		}
	}
	return nil
}

// pendingRequest is the request no run has answered: the annotation's value,
// when it is set and is not the request the record answers. It is the same
// comparison the operator makes to decide that it has a run to start, so
// "waiting" here and "will run" there cannot disagree.
func pendingRequest(annotations map[string]string, answered string) string {
	asked := annotations[AnnotationVerifyRequest]
	if strings.TrimSpace(asked) == "" || asked == answered {
		return ""
	}
	return asked
}

// RequestVerification asks the operator to verify the platform now, and
// answers with the token the run that answers will carry.
//
// It writes one annotation, with a merge patch on metadata: no spec change, so
// no new generation and no rollout. The patch names the resourceVersion the
// decision was made on, which makes it conditional — when anything wrote the
// resource in between, the operator's status included, the apiserver refuses
// it 409 and the decision is made again from a fresh read instead of over a
// change it did not see.
//
// One request waits at a time, and a second one joins it. The annotation
// holds one value, so a new token written over a waiting one would leave
// whoever asked first following a run that never comes. A request made while
// a run is in progress is not refused: it waits, and the operator starts its
// run when the current one ends — so the run that answers a request always
// began after it was made.
func (s *Service) RequestVerification(ctx context.Context) (VerifyRequest, error) {
	var err error
	for attempt := 1; attempt <= verifyAttempts; attempt++ {
		cr, note := s.zaentrum(ctx)
		if cr == nil {
			return VerifyRequest{}, fmt.Errorf("no operator instance to verify: %s", note)
		}
		v := verificationOf(*cr)
		if !v.Enabled {
			return VerifyRequest{}, fmt.Errorf("%w: spec.verification.enabled is false on %s, so the operator runs no checks — enable it to verify the platform",
				ErrVerificationDisabled, cr.Metadata.Name)
		}
		if v.PendingRequest != "" {
			return VerifyRequest{Request: v.PendingRequest, Joined: true}, nil
		}
		token := s.token()
		md := map[string]any{"annotations": map[string]string{AnnotationVerifyRequest: token}}
		if cr.Metadata.ResourceVersion != "" {
			md["resourceVersion"] = cr.Metadata.ResourceVersion
		}
		patch, _ := json.Marshal(map[string]any{"metadata": md})
		if _, err = s.k8s.PatchResource(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, cr.Metadata.Name, patch); err == nil {
			return VerifyRequest{Request: token}, nil
		}
		if !k8s.IsConflict(err) {
			return VerifyRequest{}, err
		}
	}
	return VerifyRequest{}, fmt.Errorf("the operator's resource changed on every attempt to ask for a run — try again: %w", err)
}
