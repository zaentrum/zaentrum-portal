package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// Mail.
//
// The platform's mail server is spec.mail of the operator's resource. The
// operator gives it to portal-api as its environment and says in the
// condition Mail where it stands; portal-api writes it from the mail page.
//
// The user is spec.mail.user; the password alone is not in the resource but
// in a Secret that spec.mail.secretName names. Like an addon's secret values,
// that Secret is create-only for portal-api: each new password goes into a
// new immutable Secret labelled MailSecretLabel, which the resource then
// names, so a change of the user keeps the password that is there. portal-api
// never reads, changes or deletes a Secret; the operator removes one with the
// label that the resource has not named for 10 minutes, and gives portal-api
// the name of the one it runs with (PORTAL_MAIL_SECRET).

const (
	// MailSecretLabel is on every Secret portal-api writes a mail server's
	// password into.
	MailSecretLabel = "zaentrum.io/mail"
	// MailSecretPrefix is the generateName of those Secrets.
	MailSecretPrefix = "zaentrum-mail-"
	// MailPendingSecret stands in for the name of the Secret a write stores
	// its password in, in the dry run that checks the write before that
	// Secret exists.
	MailPendingSecret = MailSecretPrefix + "pending"
	// MailSecretPassword is the one key of a mail Secret.
	MailSecretPassword = "password"
)

// The reasons of the condition Mail.
const (
	MailNotSetUp      = "NotSetUp"
	MailSecretMissing = "SecretMissing"
	MailConfigured    = "Configured"
)

// condTypeMail is the condition the operator says where mail stands in.
const condTypeMail = "Mail"

// ErrNoResource: there is no platform resource to read here — portal-api runs
// outside a cluster, or the platform is not run by the operator.
var ErrNoResource = errors.New("the platform is not run by the operator: there is no Zaentrum resource")

// ErrMailUnknown: the cluster does not know mail settings — the resource's
// schema has no spec.mail, as under an operator from before them — so a write
// of them would save nothing. SetMail wraps it with the cluster's words.
var ErrMailUnknown = errors.New("the cluster does not know spec.mail")

// MailSpec is spec.mail as the resource holds it.
type MailSpec struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	Encryption string   `json:"encryption"`
	From       string   `json:"from"`
	FromName   string   `json:"fromName"`
	User       string   `json:"user"`
	SecretName string   `json:"secretName"`
	AlertsTo   []string `json:"alertsTo"`
}

// MailCondition is the condition Mail.
type MailCondition struct {
	Status string
	Reason string
	// ObservedGeneration is the generation it was written for.
	ObservedGeneration int64
}

// Mail is what the platform's resource says about mail.
type Mail struct {
	// Resource and Generation are the resource's name and its spec's
	// generation.
	Resource   string
	Generation int64
	// Spec is spec.mail; empty when the resource has none, or one this
	// portal cannot read.
	Spec MailSpec
	// AlertMail is spec.alerts.mail; nil when the resource says nothing,
	// which is on.
	AlertMail *bool
	// Condition is the condition Mail; nil until the operator writes one.
	Condition *MailCondition
}

// Mail reads the platform's resource — the first Zaentrum in the namespace.
// ErrNoResource when there is none to read; any other error is the cluster's.
func (s *Service) Mail(ctx context.Context) (*Mail, error) {
	if !s.k8s.InCluster() {
		return nil, ErrNoResource
	}
	raw, err := s.k8s.GetResourceList(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural)
	switch {
	case k8s.IsNotFound(err):
		return nil, ErrNoResource
	case err != nil:
		return nil, err
	}
	var list zaentrumList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode the Zaentrum resources: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, ErrNoResource
	}
	cr := list.Items[0]
	m := &Mail{Resource: cr.Metadata.Name, Generation: cr.Metadata.Generation}
	if !isNull(cr.Spec.Mail) {
		var spec MailSpec
		if json.Unmarshal(cr.Spec.Mail, &spec) == nil {
			m.Spec = spec
		}
	}
	// spec.alerts is read as the alerts read it (alerts.go).
	m.AlertMail = alertsSpecOf(cr.Spec.Alerts).Mail
	for _, c := range cr.Status.Conditions {
		if c.Type == condTypeMail {
			m.Condition = &MailCondition{Status: c.Status, Reason: c.Reason, ObservedGeneration: c.ObservedGeneration}
		}
	}
	return m, nil
}

// SetMail writes spec.mail of the resource named with a JSON merge patch:
// every field of spec is set, a nil one removed, one left out kept as it is.
// It answers the generation the write made. A dry run checks the patch and
// writes nothing.
//
// The write validates strictly, so that a schema without spec.mail refuses
// it instead of dropping the field and answering 200. ErrMailUnknown, with
// the apiserver's words, for that refusal — and for an answer that holds no
// spec.mail, from an apiserver that does not validate strictly. Every other
// refusal is the apiserver's as it came, a field inside spec.mail that the
// schema does not know among them.
func (s *Service) SetMail(ctx context.Context, resource string, spec map[string]any, dryRun bool) (int64, error) {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"mail": spec}})
	if err != nil {
		return 0, err
	}
	if err := validName(resource); err != nil {
		return 0, err
	}
	raw, err := s.k8s.PatchResourceStrict(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, resource, patch, dryRun)
	if fields, words := k8s.UnknownFields(err); slices.Contains(fields, "spec.mail") {
		return 0, fmt.Errorf("%w: %s", ErrMailUnknown, words)
	}
	if err != nil {
		return 0, err
	}
	var cr struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Mail json.RawMessage `json:"mail"`
		} `json:"spec"`
	}
	if json.Unmarshal(raw, &cr) != nil {
		// The write landed when the answer is unreadable; a caller that
		// retried would write it twice.
		return 0, nil
	}
	if m := bytes.TrimSpace(cr.Spec.Mail); len(m) == 0 || m[0] != '{' {
		return 0, fmt.Errorf("%w: the apiserver answered the write without spec.mail", ErrMailUnknown)
	}
	return cr.Metadata.Generation, nil
}

// RemoveMail removes spec.mail from the resource named, and answers the
// generation that made.
func (s *Service) RemoveMail(ctx context.Context, resource string) (int64, error) {
	return s.patchGeneration(ctx, resource, []byte(`{"spec":{"mail":null}}`), false)
}

// patchGeneration applies a merge patch to the resource and answers the
// generation it made.
func (s *Service) patchGeneration(ctx context.Context, resource string, patch []byte, dryRun bool) (int64, error) {
	if err := validName(resource); err != nil {
		return 0, err
	}
	g, v, p := s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural
	var (
		raw json.RawMessage
		err error
	)
	if dryRun {
		raw, err = s.k8s.PatchResourceDryRun(ctx, g, v, p, resource, patch)
	} else {
		raw, err = s.k8s.PatchResource(ctx, g, v, p, resource, patch)
	}
	if err != nil {
		return 0, err
	}
	var cr struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
	}
	// The write landed when the answer is unreadable; a caller that retried
	// would write it twice.
	_ = json.Unmarshal(raw, &cr)
	return cr.Metadata.Generation, nil
}

// CreateMailSecret writes a mail server's password, the key password alone,
// into a new immutable Secret labelled MailSecretLabel, and answers the name
// the apiserver generated for it. It never writes to a Secret that exists.
func (s *Service) CreateMailSecret(ctx context.Context, password string) (string, error) {
	if password == "" {
		return "", errors.New("a mail Secret without a password")
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"immutable":  true,
		"metadata": map[string]any{
			"generateName": MailSecretPrefix,
			"labels":       map[string]string{MailSecretLabel: "true"},
		},
		"data": map[string]string{
			MailSecretPassword: base64.StdEncoding.EncodeToString([]byte(password)),
		},
	})
	if err != nil {
		return "", err
	}
	return s.k8s.CreateSecret(ctx, body)
}
