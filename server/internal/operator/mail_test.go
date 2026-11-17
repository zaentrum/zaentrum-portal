package operator

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// R.C2: the mail page reads spec.mail — the user with it — spec.alerts.mail
// and the condition Mail; a spec.mail this portal cannot read costs the mail page and nothing
// else — the operator console reads the resource as before — and the
// cluster's refusal is not taken for a platform without an operator.
func TestRC2MailIsReadOnItsOwn(t *testing.T) {
	s, fake := rolloutService(t)
	ctx := context.Background()
	if m, err := s.Mail(ctx); m != nil || !errors.Is(err, ErrNoResource) {
		t.Fatalf("no resource: %+v, %v", m, err)
	}
	fake.Put(zaentrums, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum", "generation": 4},
		"spec": map[string]any{
			"version": "1.4.0",
			"mail": map[string]any{"host": "smtp.example.org", "port": 465, "encryption": "tls", "from": "zaentrum@example.org",
				"user": "zaentrum@example.org", "secretName": "zaentrum-mail-x7k2p", "alertsTo": []any{"ops@example.org"}},
			"alerts": map[string]any{"mail": false},
		},
	})
	fake.SetStatus(zaentrums, "zaentrum", map[string]any{"phase": "Ready", "conditions": []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "ComponentsReady", "observedGeneration": 4},
		map[string]any{"type": "Mail", "status": "True", "reason": MailConfigured, "observedGeneration": 3},
	}})
	m, err := s.Mail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Resource != "zaentrum" || m.Generation != 4 || m.Spec.Host != "smtp.example.org" || m.Spec.Port != 465 ||
		m.Spec.User != "zaentrum@example.org" || m.Spec.SecretName != "zaentrum-mail-x7k2p" || len(m.Spec.AlertsTo) != 1 || m.AlertMail == nil || *m.AlertMail {
		t.Errorf("mail = %+v", m)
	}
	if c := m.Condition; c == nil || c.Reason != MailConfigured || c.ObservedGeneration != 3 {
		t.Errorf("condition = %+v", m.Condition)
	}

	// A port written as a word: the mail page reads no settings, the
	// operator console everything it did.
	obj := fake.Object(zaentrums, "zaentrum")
	obj["spec"].(map[string]any)["mail"] = map[string]any{"host": "smtp.example.org", "port": "five-eight-seven"}
	fake.Put(zaentrums, obj)
	if m, err := s.Mail(ctx); err != nil || m.Spec.Host != "" {
		t.Errorf("unreadable spec.mail: %+v, %v", m, err)
	}
	if info, _ := s.OperatorInfo(ctx); !info.Present || info.Version != "1.4.0" {
		t.Errorf("the operator console lost the resource: %+v", info)
	}

	fake.Forbidden[zaentrums] = true
	if _, err := s.Mail(ctx); err == nil || errors.Is(err, ErrNoResource) {
		t.Errorf("a refusal is the cluster's, not a platform without an operator: %v", err)
	}
}

// R.C2: SetMail validates strictly, in the dry run and in the write. A schema
// without spec.mail refuses it, and ErrMailUnknown carries the cluster's
// words; an apiserver that does not validate strictly drops the field and
// answers without it, which is ErrMailUnknown too. A field inside spec.mail
// that the schema does not know is the apiserver's refusal as it came.
func TestRC2SetMailValidatesStrictly(t *testing.T) {
	s, fake := rolloutService(t)
	ctx := context.Background()
	seed := func() {
		fake.Put(zaentrums, map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
			"metadata": map[string]any{"name": "zaentrum"}, "spec": map[string]any{"version": "1.4.0"}})
	}
	seed()
	spec := map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org", "user": "zaentrum@example.org"}
	if gen, err := s.SetMail(ctx, "zaentrum", spec, true); err != nil || gen != 1 {
		t.Fatalf("a dry run = %d, %v", gen, err)
	}
	if gen, err := s.SetMail(ctx, "zaentrum", spec, false); err != nil || gen != 2 {
		t.Fatalf("the write = %d, %v", gen, err)
	}
	var queries []string
	for _, c := range fake.Calls() {
		if c.Method == "PATCH" {
			queries = append(queries, c.Query)
		}
	}
	if !slices.Equal(queries, []string{"dryRun=All&fieldValidation=Strict", "fieldValidation=Strict"}) {
		t.Errorf("queries = %v", queries)
	}

	seed()
	fake.Unknown = func(_ string, obj map[string]any) []string {
		if spec, _ := obj["spec"].(map[string]any); spec["mail"] != nil {
			return []string{"spec.mail"}
		}
		return nil
	}
	for _, dry := range []bool{true, false} {
		_, err := s.SetMail(ctx, "zaentrum", spec, dry)
		if !errors.Is(err, ErrMailUnknown) || !strings.Contains(err.Error(), `strict decoding error: unknown field "spec.mail"`) {
			t.Errorf("dry run %v, a schema without spec.mail: %v", dry, err)
		}
	}
	fake.NoFieldValidation = true
	if _, err := s.SetMail(ctx, "zaentrum", spec, false); !errors.Is(err, ErrMailUnknown) || !strings.Contains(err.Error(), "without spec.mail") {
		t.Errorf("an apiserver that does not validate strictly: %v", err)
	}
	if fake.Generation(zaentrums, "zaentrum") != 1 {
		t.Errorf("generation %d: a write the cluster could not keep moved it", fake.Generation(zaentrums, "zaentrum"))
	}

	seed()
	fake.NoFieldValidation = false
	fake.Unknown = func(_ string, obj map[string]any) []string {
		spec, _ := obj["spec"].(map[string]any)
		if m, _ := spec["mail"].(map[string]any); m["user"] != nil {
			return []string{"spec.mail.user"}
		}
		return nil
	}
	_, err := s.SetMail(ctx, "zaentrum", spec, true)
	var ae *k8s.APIError
	if errors.Is(err, ErrMailUnknown) || !errors.As(err, &ae) || ae.Code != 422 || !strings.Contains(ae.Message, `unknown field "spec.mail.user"`) {
		t.Errorf("a field of spec.mail the schema does not know: %v", err)
	}
}
