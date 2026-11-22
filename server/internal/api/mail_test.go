package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak/kctest"
	zmail "github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// mailEnv is portal-api behind the test issuer, with the platform's resource
// in a fake apiserver, the realm's people in a fake Keycloak — none for an
// external identity provider — and an SMTP server on 127.0.0.1.
type mailEnv struct {
	*tokenEnv
	kc   *kctest.Server
	smtp *mailtest.Server
}

func newMailEnv(t *testing.T, bundled bool, o mailtest.Options) *mailEnv {
	t.Helper()
	e := &mailEnv{smtp: mailtest.Start(t, o)}
	if bundled {
		pe := newPeopleEnv(t)
		e.tokenEnv, e.kc = pe.tokenEnv, pe.kc
	} else {
		e.tokenEnv = newTokenEnv(t)
	}
	e.api.cfg.PublicURL = "https://zaentrum.example.org"
	return e
}

// settings are the test server's, as the operator would give them to
// portal-api.
func (e *mailEnv) settings(encryption, user, password string) zmail.Settings {
	return zmail.Settings{
		Host: e.smtp.Host, Port: e.smtp.Port, Encryption: encryption, From: "zaentrum@example.org", FromName: "Our server",
		AlertsTo: []string{"ops@example.org"}, Username: user, Password: password,
	}
}

// specOf is spec.mail for the settings — their user with them — naming
// secret.
func specOf(s zmail.Settings, secret string) map[string]any {
	m := map[string]any{"host": s.Host, "port": s.Port, "encryption": s.Encryption, "from": s.From, "fromName": s.FromName}
	if s.Username != "" {
		m["user"] = s.Username
	}
	if len(s.AlertsTo) > 0 {
		m["alertsTo"] = s.AlertsTo
	}
	if secret != "" {
		m["secretName"] = secret
	}
	return m
}

// putPlatform seeds the platform's resource with spec.mail (nil: none) and
// the condition Mail (reason "": none) for its generation.
func (e *mailEnv) putPlatform(spec map[string]any, reason string) {
	s := map[string]any{"version": "1.4.0", "channel": "stable", "hostname": "zaentrum.example.org", "features": map[string]any{"pipeline": true}}
	if spec != nil {
		s["mail"] = spec
	}
	e.kube.Put("zaentrums", map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum", "metadata": map[string]any{"name": "zaentrum"}, "spec": s})
	if reason != "" {
		e.setCondition(reason, 1)
	}
}

func (e *mailEnv) setCondition(reason string, generation int) {
	status := "False"
	if reason == operator.MailConfigured {
		status = "True"
	}
	e.kube.SetStatus("zaentrums", "zaentrum", map[string]any{"phase": "Ready", "conditions": []any{map[string]any{
		"type": "Mail", "status": status, "reason": reason, "message": "", "observedGeneration": generation,
		"lastTransitionTime": "2026-11-15T10:00:00Z",
	}}})
}

// ready makes mail ready: the resource names the test server, the operator
// says Configured, and portal-api runs with the same settings and the
// password of the Secret named.
func (e *mailEnv) ready(encryption string) {
	e.readyAs(encryption, "zaentrum@example.org", "password-for-tests", "zaentrum-mail-kept1")
}

// readyAs is ready with the sign-in of user, its password in secret; "" and
// "" for none.
func (e *mailEnv) readyAs(encryption, user, password, secret string) {
	s := e.settings(encryption, user, password)
	e.api.cfg.Mail, e.api.cfg.MailSecret = s, secret
	e.putPlatform(specOf(s, secret), operator.MailConfigured)
}

func (e *mailEnv) mailSpec() map[string]any {
	e.t.Helper()
	spec, _ := e.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any)
	m, _ := spec["mail"].(map[string]any)
	return m
}

func (e *mailEnv) getMail() mailDoc {
	e.t.Helper()
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/mail", nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /mail = %d %s", rec.Code, rec.Body)
	}
	return decodeAs[mailDoc](e.t, rec)
}

// writes are the calls that changed the cluster.
func (e *mailEnv) writes() []k8sfake.Call {
	var out []k8sfake.Call
	for _, c := range e.kube.Calls() {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// R.C2: the four mail routes are the admins', on a token of the portal's
// own clients, as every console's.
func TestRC2MailRoutesAreTheAdmins(t *testing.T) {
	e := newMailEnv(t, false, mailtest.Options{})
	e.ready(zmail.None)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/portal/mail"}, {http.MethodPut, "/api/portal/mail"},
		{http.MethodDelete, "/api/portal/mail"}, {http.MethodPost, "/api/portal/mail/test"},
	}
	refused := map[string]struct {
		claims map[string]any
		code   int
	}{
		"no bearer":                      {nil, http.StatusUnauthorized},
		"a viewer":                       {viewerPortal, http.StatusForbidden},
		"an admin through the media app": {adminMedia, http.StatusForbidden},
		"an addon's service account":     {addonSample, http.StatusForbidden},
	}
	body := map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org", "to": "mia@example.org"}
	for _, rt := range routes {
		for name, c := range refused {
			if rec := e.do(c.claims, rt.method, rt.path, body); rec.Code != c.code {
				t.Errorf("%s %s as %s = %d, want %d", rt.method, rt.path, name, rec.Code, c.code)
			}
		}
	}
	if n := len(e.writes()); n != 0 || len(e.kube.SecretNames()) != 0 || len(e.smtp.Sessions()) != 0 {
		t.Errorf("refused callers wrote %d times, made %d Secrets, opened %d connections", n, len(e.kube.SecretNames()), len(e.smtp.Sessions()))
	}
	for _, rt := range routes {
		if rec := e.do(adminCLI, rt.method, rt.path, nil); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("%s %s as the admin through the CLI = %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
}

// R.C2: GET /mail answers the settings without the password, passwordSet,
// setUp, ready, secretMissing, runByOperator, identity, alertMail and
// recipients — those fields exactly.
func TestRC2GetMail(t *testing.T) {
	t.Run("R.C2 not set up", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, operator.MailNotSetUp)
		rec := e.do(adminPortal, http.MethodGet, "/api/portal/mail", nil)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		want := []string{"alertMail", "alertsTo", "encryption", "from", "fromName", "host", "identity", "passwordSet", "port", "ready", "recipients", "runByOperator", "secretMissing", "setUp", "user"}
		if !slices.Equal(keys, want) {
			t.Errorf("fields = %v, want %v", keys, want)
		}
		d := decodeAs[mailDoc](t, rec)
		if d.SetUp || d.Ready || d.PasswordSet || d.Host != "" || d.From != "" || d.User != "" || d.Identity != identityBundled || !d.AlertMail ||
			d.SecretMissing || !d.RunByOperator {
			t.Errorf("not set up: %+v", d)
		}
		// The form empty, with port 587 and STARTTLS: the settings' defaults.
		if d.Port != 587 || d.Encryption != zmail.StartTLS || d.FromName != "zaentrum" || d.AlertsTo == nil {
			t.Errorf("defaults: %+v", d)
		}
	})
	t.Run("R.C2 ready, with every admin and the further addresses", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.StartTLS)
		// The import's placeholder counts as no address; a person who is not
		// an admin is not listed.
		e.kc.Add(kctest.Seed{Username: "first", FirstName: "First", LastName: "Admin", Email: "admin@zaentrum.local", Roles: []string{kctest.AdminRole, kctest.UserRole}})
		e.kc.Add(kctest.Seed{Username: "mia", FirstName: "Mia", Email: "mia@example.org", Roles: []string{kctest.UserRole}})
		d := e.getMail()
		if !d.SetUp || !d.Ready || !d.PasswordSet || d.User != "zaentrum@example.org" || d.Host != e.smtp.Host || d.Port != e.smtp.Port ||
			d.From != "zaentrum@example.org" || d.FromName != "Our server" || !slices.Equal(d.AlertsTo, []string{"ops@example.org"}) {
			t.Errorf("ready: %+v", d)
		}
		want := []mailAdmin{{"Anna", ""}, {"First Admin", ""}, {"Zaentrum Administrator", "owner@example.org"}}
		if !slices.Equal(d.Recipients.Admins, want) || !slices.Equal(d.Recipients.Further, []string{"ops@example.org"}) {
			t.Errorf("recipients = %+v", d.Recipients)
		}
		if rec := e.do(adminPortal, http.MethodGet, "/api/portal/mail", nil); strings.Contains(rec.Body.String(), "password-for-tests") {
			t.Error("GET /mail carries the password")
		}
	})
	t.Run("R.C2 not ready until the operator and portal-api both have the settings", func(t *testing.T) {
		for name, unready := range map[string]func(e *mailEnv){
			"portal-api runs with older settings": func(e *mailEnv) { e.api.cfg.Mail.Host = "smtp.example.org" },
			"the Secret is missing":               func(e *mailEnv) { e.setCondition(operator.MailSecretMissing, 1) },
			// A new password is a new Secret: portal-api runs with the old
			// one's until it is given the new one.
			"a new password, not yet given to portal-api": func(e *mailEnv) { e.api.cfg.MailSecret = "zaentrum-mail-older" },
			"a new user, not yet given to portal-api":     func(e *mailEnv) { e.api.cfg.Mail.Username = "older@example.org" },
			"an operator that says nothing of mail":       func(e *mailEnv) { e.kube.SetStatus("zaentrums", "zaentrum", map[string]any{"phase": "Ready"}) },
			// A condition the operator wrote for an older generation says
			// nothing of this one.
			"the condition is of an older generation": func(e *mailEnv) {
				obj := e.kube.Object("zaentrums", "zaentrum")
				obj["metadata"].(map[string]any)["generation"] = 2
				e.kube.Put("zaentrums", obj)
			},
		} {
			e := newMailEnv(t, true, mailtest.Options{})
			e.ready(zmail.StartTLS)
			if d := e.getMail(); !d.Ready {
				t.Fatalf("%s: not ready before: %+v", name, d)
			}
			unready(e)
			// The settings answered are the resource's, the user among them.
			if d := e.getMail(); !d.SetUp || d.Ready || d.Host != e.smtp.Host || d.User != "zaentrum@example.org" || !d.PasswordSet {
				t.Errorf("%s: %+v", name, d)
			}
		}
	})
	t.Run("R.C2 alerts not mailed", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.StartTLS)
		obj := e.kube.Object("zaentrums", "zaentrum")
		obj["spec"].(map[string]any)["alerts"] = map[string]any{"mail": false}
		e.kube.Put("zaentrums", obj)
		if d := e.getMail(); d.AlertMail {
			t.Errorf("spec.alerts.mail false: %+v", d)
		}
	})
	t.Run("R.C2 an external identity provider", func(t *testing.T) {
		e := newMailEnv(t, false, mailtest.Options{})
		e.ready(zmail.StartTLS)
		d := e.getMail()
		if d.Identity != identityExternal || d.Recipients.Admins == nil || len(d.Recipients.Admins) != 0 ||
			!slices.Equal(d.Recipients.Further, []string{"ops@example.org"}) {
			t.Errorf("external: %+v", d)
		}
	})
	t.Run("R.C2 not run by the operator: the settings portal-api runs with", func(t *testing.T) {
		e := newMailEnv(t, false, mailtest.Options{})
		e.api.cfg.Mail = e.settings(zmail.TLS, "zaentrum@example.org", "password-for-tests")
		d := e.getMail()
		if !d.SetUp || !d.Ready || !d.PasswordSet || d.Encryption != zmail.TLS || d.User != "zaentrum@example.org" || d.RunByOperator || d.SecretMissing {
			t.Errorf("from the chart's values: %+v", d)
		}
	})
}

// R.C2: PUT /mail writes spec.mail into the platform's resource, as the
// operator console patches it, and a new password into a new Secret labelled
// zaentrum.io/mail, which the resource then names — the way an addon's secret
// values are kept; the user goes into spec.mail, so a change of the user keeps
// the password. It answers the resource's new generation.
func TestRC2PutMail(t *testing.T) {
	body := func(password any) map[string]any {
		b := map[string]any{
			"host": "smtp.example.org", "port": 587, "encryption": "starttls", "from": "zaentrum@example.org",
			"fromName": "Our server", "user": "zaentrum@example.org", "alertsTo": []string{"ops@example.org", "OPS@example.org"},
		}
		if password != nil {
			b["password"] = password
		}
		return b
	}
	t.Run("R.C2 a new password goes into a new Secret, which the resource then names", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, operator.MailNotSetUp)
		rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", body("password-for-tests"))
		if rec.Code != http.StatusOK || e.decodeGeneration(rec) != 2 {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
		}
		names := e.kube.SecretNames()
		if len(names) != 1 || !strings.HasPrefix(names[0], operator.MailSecretPrefix) {
			t.Fatalf("secrets = %v", names)
		}
		spec := e.mailSpec()
		want := map[string]any{"host": "smtp.example.org", "port": json.Number("587"), "encryption": "starttls", "from": "zaentrum@example.org",
			"fromName": "Our server", "user": "zaentrum@example.org", "alertsTo": []any{"ops@example.org"}, "secretName": names[0]}
		if !jsonEqual(spec, want) {
			t.Errorf("spec.mail = %v, want %v", spec, want)
		}
		sec := e.kube.Secret(names[0])
		labels, _ := sec["metadata"].(map[string]any)["labels"].(map[string]any)
		if sec["immutable"] != true || labels[operator.MailSecretLabel] != "true" {
			t.Errorf("secret = %v", sec)
		}
		if got := secretData(t, e.kube, names[0]); got["password"] != "password-for-tests" || len(got) != 1 {
			t.Errorf("secret data = %v, want the key password alone", got)
		}
		// Checked in a dry run first, then the Secret, then the write; the
		// platform's other settings stay.
		var steps []string
		for _, c := range e.writes() {
			steps = append(steps, c.Method+" "+c.Query)
		}
		if !slices.Equal(steps, []string{"PATCH dryRun=All&fieldValidation=Strict", "POST ", "PATCH fieldValidation=Strict"}) {
			t.Errorf("writes = %v", steps)
		}
		if dry := e.writes()[0].Body; !strings.Contains(dry, operator.MailPendingSecret) || strings.Contains(dry, "password-for-tests") {
			t.Errorf("the dry run = %s", dry)
		}
		obj := e.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any)
		if obj["version"] != "1.4.0" || obj["features"].(map[string]any)["pipeline"] != true {
			t.Errorf("the rest of the spec changed: %v", obj)
		}
	})
	t.Run("R.C2 a password left out: the one kept stays", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.StartTLS)
		for _, password := range []any{nil, ""} {
			b := body(password)
			b["host"] = "smtp2.example.org"
			if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", b); rec.Code != http.StatusOK {
				t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
			}
			if spec := e.mailSpec(); spec["secretName"] != "zaentrum-mail-kept1" || spec["host"] != "smtp2.example.org" {
				t.Errorf("spec.mail = %v", spec)
			}
		}
		if names := e.kube.SecretNames(); len(names) != 0 {
			t.Errorf("a password left out made Secrets %v", names)
		}
	})
	t.Run("R.C2 a change of the user keeps the password", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.StartTLS)
		for _, user := range []string{"another@example.org", ""} {
			b := body(nil)
			b["user"] = user
			if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", b); rec.Code != http.StatusOK {
				t.Fatalf("user %q: PUT = %d %s", user, rec.Code, rec.Body)
			}
			spec := e.mailSpec()
			if got, _ := spec["user"].(string); got != user || spec["secretName"] != "zaentrum-mail-kept1" {
				t.Errorf("user %q: spec.mail = %v", user, spec)
			}
			if d := e.getMail(); d.User != user || !d.PasswordSet {
				t.Errorf("user %q: GET = %+v", user, d)
			}
		}
		if names := e.kube.SecretNames(); len(names) != 0 {
			t.Errorf("a change of the user made Secrets %v", names)
		}
		// A password typed with no user is a new password all the same.
		b := body("password-for-tests")
		b["user"] = ""
		if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", b); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
		}
		if names := e.kube.SecretNames(); len(names) != 1 || e.mailSpec()["secretName"] != names[0] || e.mailSpec()["user"] != nil {
			t.Errorf("secrets %v, spec.mail %v", names, e.mailSpec())
		}
	})
	t.Run("R.C2 a user needs a password where none is kept", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.readyAs(zmail.StartTLS, "", "", "")
		rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", body(nil))
		refused(t, rec, http.StatusBadRequest, "mail_user_needs_password", "Type the password of this user.")
		if len(e.writes()) != 0 {
			t.Errorf("a refused PUT wrote %v", e.writes())
		}
	})
	t.Run("R.C2 the defaults are left to the resource", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, "")
		if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org"}); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
		}
		if spec := e.mailSpec(); !jsonEqual(spec, map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org"}) {
			t.Errorf("spec.mail = %v", spec)
		}
	})
	t.Run("R.C2 refused settings write nothing", func(t *testing.T) {
		eleven := make([]string, 11)
		for i := range eleven {
			eleven[i] = string(rune('a'+i)) + "@example.org"
		}
		for _, c := range []struct {
			name, field string
			value       any
			code, say   string
		}{
			{"no server", "host", "", "mail_server_missing", "Type the name of the mail server."},
			{"a server with a port", "host", "smtp.example.org:587", "mail_server_invalid", "The server is not a host name or an IP address."},
			{"a server with a space", "host", "smtp example.org", "mail_server_invalid", "The server is not a host name or an IP address."},
			{"a port out of range", "port", 70000, "mail_port_invalid", "The port is a number from 1 to 65535."},
			{"an encryption that is none", "encryption", "ssl", "mail_encryption_invalid", "Encryption is starttls, tls or none."},
			{"no sender", "from", "", "mail_sender_missing", "Type the sender's address."},
			{"a sender that is no address", "from", "zaentrum", "mail_sender_invalid", "The sender's address is not an e-mail address."},
			{"a sender with a name", "from", "Our server <zaentrum@example.org>", "mail_sender_invalid", "The sender's address is not an e-mail address."},
			{"a sender's name too long", "fromName", strings.Repeat("n", 61), "mail_sender_name_too_long", "The sender's name has at most 60 characters."},
			{"eleven further addresses", "alertsTo", eleven, "mail_further_address_limit", "There are more than 10 further addresses. Keep up to 10."},
			{"a further address that is none", "alertsTo", []string{"ops"}, "mail_further_address_invalid", "One of the further addresses is not an e-mail address."},
		} {
			t.Run("R.C2 "+c.name, func(t *testing.T) {
				e := newMailEnv(t, true, mailtest.Options{})
				e.putPlatform(nil, "")
				b := body("password-for-tests")
				b[c.field] = c.value
				rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", b)
				if got := refused(t, rec, http.StatusBadRequest, c.code, c.say); got.Values != nil || got.Detail != "" {
					t.Errorf("a refusal of save names nothing more: %+v", got)
				}
				if len(e.writes()) != 0 || len(e.kube.SecretNames()) != 0 {
					t.Errorf("a refused PUT wrote %v", e.writes())
				}
			})
		}
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, "")
		b := body("password-for-tests")
		b["secretName"] = "zaentrum-mail-mine"
		if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", b); rec.Code != http.StatusBadRequest || len(e.writes()) != 0 {
			t.Errorf("a field PUT does not take = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("R.C2 a write the cluster refuses leaves no Secret behind", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, "")
		e.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
			if c.Method == http.MethodPatch && strings.Contains(c.Query, "dryRun=All") {
				return http.StatusUnprocessableEntity, "spec.mail.host: Invalid value", true
			}
			return 0, "", false
		}
		rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", body("password-for-tests"))
		got := refused(t, rec, http.StatusUnprocessableEntity, "mail_write_refused", "The cluster refused the mail settings.")
		if !strings.Contains(got.Detail, "spec.mail.host: Invalid value") {
			t.Errorf("the cluster's own words go under details: %+v", got)
		}
		if len(e.kube.SecretNames()) != 0 || e.mailSpec() != nil {
			t.Errorf("secrets %v, spec.mail %v", e.kube.SecretNames(), e.mailSpec())
		}
	})
	t.Run("R.C2 not run by the operator", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			rec := e.do(adminPortal, method, "/api/portal/mail", body("password-for-tests"))
			refused(t, rec, http.StatusConflict, "mail_not_run_by_operator", "This platform is not run by the operator. Mail is set in its chart's values.")
		}
		if len(e.writes()) != 0 || len(e.kube.SecretNames()) != 0 {
			t.Errorf("writes %v", e.writes())
		}
	})
}

// refused checks a refusal of a mail route: its status, its code and the
// sentence of the Words it carries, as JSON. It answers the refusal.
func refused(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) refusal {
	t.Helper()
	var got refusal
	if rec.Code != status || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") ||
		json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Error != code || got.Message != message {
		t.Errorf("answer = %d %s %s, want %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body, status, code, message)
	}
	return got
}

func (e *mailEnv) decodeGeneration(rec *httptest.ResponseRecorder) int64 {
	e.t.Helper()
	var out struct {
		Generation int64 `json:"generation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Generation
}

// jsonEqual compares two JSON values by what they encode to.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	var u, v any
	_ = json.Unmarshal(x, &u)
	_ = json.Unmarshal(y, &v)
	return string(must(json.Marshal(u))) == string(must(json.Marshal(v)))
}

func must(b []byte, _ error) []byte { return b }

// R.C2: DELETE /mail removes spec.mail, and nothing else of the resource; it
// answers the generation that made.
func TestRC2DeleteMail(t *testing.T) {
	e := newMailEnv(t, true, mailtest.Options{})
	e.ready(zmail.StartTLS)
	rec := e.do(adminPortal, http.MethodDelete, "/api/portal/mail", nil)
	if rec.Code != http.StatusOK || e.decodeGeneration(rec) != 2 {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	spec := e.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any)
	if _, ok := spec["mail"]; ok || spec["version"] != "1.4.0" || spec["hostname"] != "zaentrum.example.org" {
		t.Errorf("spec = %v", spec)
	}
	if len(e.kube.SecretNames()) != 0 {
		t.Error("DELETE made a Secret")
	}
	if d := e.getMail(); d.SetUp || d.Ready {
		t.Errorf("after DELETE: %+v", d)
	}
}

// R.C2: POST /mail/test sends the test mail, with the words of the section
// Words, while the admin waits: 200 once the server took it, 502 with the
// reason's code and sentence; while mail is not set up nothing is sent and the
// answer says so. The log says a mail of a kind was sent or refused, never an
// address, never the text.
func TestRC2TestMail(t *testing.T) {
	to := map[string]any{"to": "mia@example.org"}
	t.Run("R.C2 not set up: nothing is sent, and the answer says so", func(t *testing.T) {
		for _, bundled := range []bool{true, false} {
			e := newMailEnv(t, bundled, mailtest.Options{})
			e.putPlatform(nil, operator.MailNotSetUp)
			e.api.cfg.Mail = e.settings(zmail.None, "", "") // the resource says no more, whatever portal-api still runs with
			rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
			refused(t, rec, http.StatusConflict, "mail_not_set_up", "Mail is not set up.")
			if len(e.smtp.Sessions()) != 0 {
				t.Errorf("bundled %v: a connection was opened", bundled)
			}
		}
	})
	t.Run("R.C2 saved, not ready yet", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.None)
		e.api.cfg.Mail = zmail.Settings{}
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
		refused(t, rec, http.StatusConflict, "mail_not_ready", "The mail settings are still being applied.")
		// A new password: portal-api still runs with the Secret before it.
		e.ready(zmail.None)
		e.api.cfg.MailSecret = "zaentrum-mail-older"
		rec = e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
		refused(t, rec, http.StatusConflict, "mail_not_ready", "The mail settings are still being applied.")
		e.ready(zmail.None)
		e.setCondition(operator.MailSecretMissing, 1)
		rec = e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
		refused(t, rec, http.StatusConflict, "mail_password_missing", "The mail server's password is missing.")
		if len(e.smtp.Sessions()) != 0 {
			t.Error("a connection was opened")
		}
	})
	t.Run("R.C2 ready: the test mail is sent", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{Auth: []string{"PLAIN"}, User: "zaentrum@example.org", Password: "password-for-tests"})
		e.ready(zmail.None)
		logged := captureLog(t)
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /mail/test = %d %s", rec.Code, rec.Body)
		}
		if got := decodeAs[map[string]string](t, rec); got["to"] != "mia@example.org" || got["message"] != "A test mail was sent to mia@example.org." {
			t.Errorf("answer = %v", got)
		}
		mails := e.smtp.Mails()
		if len(mails) != 1 || !slices.Equal(mails[0].To, []string{"mia@example.org"}) || mails[0].User != "zaentrum@example.org" {
			t.Fatalf("mails = %+v", mails)
		}
		msg, err := mail.ReadMessage(bytes.NewReader(mails[0].Data))
		if err != nil {
			t.Fatal(err)
		}
		text, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
		if msg.Header.Get("Subject") != "Test mail from zaentrum.example.org" ||
			strings.TrimSpace(string(text)) != "This mail was sent from the mail page of zaentrum.example.org, to check that mail works. Nothing else needs doing." {
			t.Errorf("subject %q, text %q", msg.Header.Get("Subject"), text)
		}
		if !strings.Contains(logged.String(), "mail: a test mail was sent") {
			t.Errorf("log = %q", logged)
		}
		for _, secret := range []string{"mia@example.org", "zaentrum@example.org", "mail page of", "password-for-tests"} {
			if strings.Contains(logged.String(), secret) {
				t.Errorf("the log holds %q: %q", secret, logged)
			}
		}
	})
	t.Run("R.C2 the server refuses: 502, with the reason's code and sentence", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{Replies: map[string]string{"RCPT": "550 5.1.1 <mia@example.org>: no such user"}})
		e.readyAs(zmail.None, "", "", "")
		logged := captureLog(t)
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
		got := refused(t, rec, http.StatusBadGateway, "mail_recipient_refused", "The mail server refused the address mia@example.org.")
		if got.Values["address"] != "mia@example.org" || len(got.Values) != 1 {
			t.Errorf("values = %v, want the address the sentence names", got.Values)
		}
		if !strings.Contains(logged.String(), "mail: a test mail was refused (recipient)") || strings.Contains(logged.String(), "example.org") {
			t.Errorf("log = %q", logged)
		}
	})
	t.Run("R.C2 the certificate is checked against the authorities the image trusts", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{TLS: true})
		e.ready(zmail.TLS)
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
		refused(t, rec, http.StatusBadGateway, "mail_certificate_unverified", "The mail server's certificate could not be checked.")
	})
	t.Run("R.C2 starttls to a server that does not offer it", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{Auth: []string{"PLAIN"}, User: "zaentrum@example.org", Password: "password-for-tests"})
		e.ready(zmail.StartTLS)
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to)
		refused(t, rec, http.StatusBadGateway, "mail_encryption_not_offered", "The mail server doesn't offer this encryption. Choose another.")
		for _, sess := range e.smtp.Sessions() {
			for _, c := range sess {
				if c.Verb != "EHLO" {
					t.Errorf("%s was said in the clear", c.Verb)
				}
			}
		}
	})
	t.Run("R.C2 an address alone, and never the placeholder", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.None)
		for _, addr := range []string{"", "mia", "Mia <mia@example.org>"} {
			rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", map[string]any{"to": addr})
			refused(t, rec, http.StatusBadRequest, "address_invalid", "This is not an e-mail address.")
		}
		for _, addr := range []string{"admin@zaentrum.local", "Admin@Zaentrum.Local"} {
			rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", map[string]any{"to": addr})
			refused(t, rec, http.StatusBadRequest, "address_placeholder", "This address is a placeholder: nothing is sent to it. Type another.")
		}
		if len(e.smtp.Sessions()) != 0 {
			t.Error("a connection was opened")
		}
	})
	t.Run("R.C2 not run by the operator: sent with the chart's values", func(t *testing.T) {
		e := newMailEnv(t, false, mailtest.Options{})
		e.api.cfg.Mail = e.settings(zmail.None, "", "")
		if rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to); rec.Code != http.StatusOK || len(e.smtp.Mails()) != 1 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
}

// R.C2: a refusal the cluster caused says the case in the reader's words, and
// the cluster's own words go apart, under details: a read the cluster forbids,
// a write it forbids, and any other answer it gives.
func TestRC2TheClustersRefusalsKeepItsWordsApart(t *testing.T) {
	failing := func(method string, code int) func(k8sfake.Call) (int, string, bool) {
		return func(c k8sfake.Call) (int, string, bool) {
			if c.Method == method && strings.Contains(c.Path, "zaentrums") {
				return code, "the cluster's own words", true
			}
			return 0, "", false
		}
	}
	for _, c := range []struct {
		name, method, route string
		fail                func(k8sfake.Call) (int, string, bool)
		status              int
		code, say           string
	}{
		{"R.C2 a read the cluster forbids", http.MethodGet, "/api/portal/mail", failing(http.MethodGet, http.StatusForbidden),
			http.StatusServiceUnavailable, "platform_read_forbidden",
			"The portal is not allowed to read the platform's settings. Update the operator, which gives the portal that right."},
		{"R.C2 a read the cluster forbids, before a test mail", http.MethodPost, "/api/portal/mail/test", failing(http.MethodGet, http.StatusForbidden),
			http.StatusServiceUnavailable, "platform_read_forbidden",
			"The portal is not allowed to read the platform's settings. Update the operator, which gives the portal that right."},
		{"R.C2 a write the cluster forbids", http.MethodPut, "/api/portal/mail", failing(http.MethodPatch, http.StatusForbidden),
			http.StatusServiceUnavailable, "mail_write_forbidden",
			"The portal is not allowed to change the mail settings. Update the operator, which gives the portal that right."},
		{"R.C2 a removal the cluster forbids", http.MethodDelete, "/api/portal/mail", failing(http.MethodPatch, http.StatusForbidden),
			http.StatusServiceUnavailable, "mail_write_forbidden",
			"The portal is not allowed to change the mail settings. Update the operator, which gives the portal that right."},
		{"R.C2 a read that fails otherwise", http.MethodGet, "/api/portal/mail", failing(http.MethodGet, http.StatusInternalServerError),
			http.StatusBadGateway, "cluster_unexpected_answer", "The cluster did not answer as expected."},
		{"R.C2 a write that fails otherwise", http.MethodPut, "/api/portal/mail", failing(http.MethodPatch, http.StatusInternalServerError),
			http.StatusBadGateway, "cluster_unexpected_answer", "The cluster did not answer as expected."},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newMailEnv(t, true, mailtest.Options{})
			e.ready(zmail.None)
			e.kube.Fail = c.fail
			var b any
			switch c.method {
			case http.MethodPut:
				b = map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org"}
			case http.MethodPost:
				b = map[string]any{"to": "mia@example.org"}
			}
			got := refused(t, e.do(adminPortal, c.method, c.route, b), c.status, c.code, c.say)
			if !strings.Contains(got.Detail, "the cluster's own words") || strings.Contains(got.Message, "the cluster's own words") {
				t.Errorf("the cluster's words go under details, never into the message: %+v", got)
			}
		})
	}
}

// R.C2: a link in a mail and the platform's name in it come from the public
// address alone. A portal-api without a full one — http or https, and a host —
// sends no mail: where the operator runs the platform mail is not ready, and
// where it does not, portal-api's own settings are not set up.
func TestRC2NoMailWithoutAFullPublicAddress(t *testing.T) {
	to := map[string]any{"to": "mia@example.org"}
	for _, public := range []string{"", "   ", "zaentrum.example.org", "//zaentrum.example.org", "ftp://zaentrum.example.org", "https://"} {
		t.Run(fmt.Sprintf("R.C2 run by the operator, PORTAL_PUBLIC_URL %q", public), func(t *testing.T) {
			e := newMailEnv(t, true, mailtest.Options{})
			e.ready(zmail.None)
			e.api.cfg.PublicURL = public
			if d := e.getMail(); !d.SetUp || d.Ready {
				t.Errorf("GET = %+v, want set up and not ready", d)
			}
			refused(t, e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to), http.StatusConflict,
				"mail_not_ready", "The mail settings are still being applied.")
			if n := len(e.smtp.Sessions()); n != 0 {
				t.Errorf("%d connections to the mail server", n)
			}
		})
		t.Run(fmt.Sprintf("R.C2 not run by the operator, PORTAL_PUBLIC_URL %q", public), func(t *testing.T) {
			e := newMailEnv(t, false, mailtest.Options{})
			e.api.cfg.Mail = e.settings(zmail.None, "", "")
			e.api.cfg.PublicURL = public
			if d := e.getMail(); d.SetUp || d.Ready {
				t.Errorf("GET = %+v, want not set up", d)
			}
			refused(t, e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to), http.StatusConflict,
				"mail_not_set_up", "Mail is not set up.")
			if n := len(e.smtp.Sessions()); n != 0 {
				t.Errorf("%d connections to the mail server", n)
			}
		})
	}
}

// R.C2: the platform's name in the test mail is its public address's host,
// whatever name and forwarded headers the request that asked for it came with.
func TestRC2TheTestMailNamesThePublicAddressNotTheRequest(t *testing.T) {
	e := newMailEnv(t, true, mailtest.Options{})
	e.readyAs(zmail.None, "", "", "")
	raw, _ := json.Marshal(map[string]any{"to": "mia@example.org"})
	req := httptest.NewRequest(http.MethodPost, "/api/portal/mail/test", bytes.NewReader(raw))
	req.Host = "elsewhere.example.org"
	req.Header.Set("Authorization", "Bearer "+e.iss.Token(t, adminPortal))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "elsewhere.example.org")
	req.Header.Set("Origin", "https://elsewhere.example.org")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mail/test = %d %s", rec.Code, rec.Body)
	}
	mails := e.smtp.Mails()
	if len(mails) != 1 {
		t.Fatalf("mails = %+v", mails)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(mails[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if msg.Header.Get("Subject") != "Test mail from zaentrum.example.org" || strings.Contains(string(text), "elsewhere") {
		t.Errorf("subject %q, text %q: the platform's name is its public address's", msg.Header.Get("Subject"), text)
	}
}

// R.C2: mail is ready only where portal-api runs with the password whenever a
// user is set — on the platform the operator runs and on one it does not run
// alike. Without it nothing is sent: the test mail says the password is
// missing, and no connection is opened.
func TestRC2ReadyNeedsThePasswordWhereAUserIsSet(t *testing.T) {
	to := map[string]any{"to": "mia@example.org"}
	for name, prepare := range map[string]func(e *mailEnv){
		"R.C2 run by the operator": func(e *mailEnv) {
			e.ready(zmail.None)
			e.api.cfg.Mail.Password = ""
		},
		"R.C2 not run by the operator": func(e *mailEnv) {
			e.api.cfg.Mail = e.settings(zmail.None, "zaentrum@example.org", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newMailEnv(t, false, mailtest.Options{Auth: []string{"PLAIN"}, User: "zaentrum@example.org", Password: ""})
			prepare(e)
			if d := e.getMail(); !d.SetUp || d.Ready {
				t.Errorf("GET = %+v, want set up and not ready", d)
			}
			refused(t, e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", to), http.StatusConflict,
				"mail_password_missing", "The mail server's password is missing.")
			if n := len(e.smtp.Sessions()); n != 0 {
				t.Errorf("%d connections to the mail server", n)
			}
			// Without a user there is no sign-in, and nothing to miss.
			e.api.cfg.Mail.Username = ""
			if e.api.resourceSpec() != nil {
				e.putPlatform(specOf(e.api.cfg.Mail, e.api.cfg.MailSecret), operator.MailConfigured)
			}
			if d := e.getMail(); !d.Ready {
				t.Errorf("without a user: %+v", d)
			}
		})
	}
}

// resourceSpec is spec.mail of the platform's resource where the test made
// one, nil where it made none.
func (a *API) resourceSpec() *operator.MailSpec {
	m, err := a.op.Mail(context.Background())
	if err != nil {
		return nil
	}
	return &m.Spec
}

// R.C2: where the cluster does not know mail settings — the resource's schema
// has no spec.mail, as under an operator from before them — save writes
// nothing, makes no Secret and says so, with the mail page's sentence, the
// status of the other "Update the operator" refusals and the cluster's own
// words under details. The write is checked in a dry run that validates
// strictly, which the apiserver refuses for the unknown field; an apiserver
// that does not validate strictly drops the field and answers without it,
// and that answer is refused the same way.
func TestRC2AClusterThatDoesNotKnowMailSettings(t *testing.T) {
	withoutMail := func(plural string, obj map[string]any) []string {
		if spec, _ := obj["spec"].(map[string]any); plural == "zaentrums" && spec["mail"] != nil {
			return []string{"spec.mail"}
		}
		return nil
	}
	for _, c := range []struct {
		name   string
		strict bool
		detail string
	}{
		{"R.C2 the apiserver refuses the unknown field", true, `strict decoding error: unknown field "spec.mail"`},
		{"R.C2 an apiserver that does not validate strictly drops it", false, "the apiserver answered the write without spec.mail"},
	} {
		for _, password := range []any{"password-for-tests", nil} {
			t.Run(fmt.Sprintf("%s, password %v", c.name, password), func(t *testing.T) {
				e := newMailEnv(t, true, mailtest.Options{})
				e.putPlatform(nil, "")
				e.kube.Unknown, e.kube.NoFieldValidation = withoutMail, !c.strict
				before := e.kube.Object("zaentrums", "zaentrum")
				b := map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org"}
				if password != nil {
					b["password"] = password
				}
				got := refused(t, e.do(adminPortal, http.MethodPut, "/api/portal/mail", b), http.StatusServiceUnavailable,
					"mail_settings_unknown", "The cluster does not know mail settings. Update the operator, which brings them.")
				if !strings.Contains(got.Detail, c.detail) || strings.Contains(got.Message, "strict") {
					t.Errorf("the cluster's words go under details: %+v", got)
				}
				var steps []string
				for _, w := range e.writes() {
					steps = append(steps, w.Method+" "+w.Query)
				}
				if !slices.Equal(steps, []string{"PATCH dryRun=All&fieldValidation=Strict"}) || len(e.kube.SecretNames()) != 0 {
					t.Errorf("writes %v, secrets %v: the dry run alone, and no Secret", steps, e.kube.SecretNames())
				}
				if after := e.kube.Object("zaentrums", "zaentrum"); !jsonEqual(after, before) {
					t.Errorf("the resource changed: %v", after)
				}
			})
		}
	}
	t.Run("R.C2 a mail setting the cluster does not know is the cluster's refusal", func(t *testing.T) {
		// A schema with spec.mail and without a field of it.
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, "")
		e.kube.Unknown = func(_ string, obj map[string]any) []string {
			spec, _ := obj["spec"].(map[string]any)
			if m, _ := spec["mail"].(map[string]any); m["user"] != nil {
				return []string{"spec.mail.user"}
			}
			return nil
		}
		b := map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org", "user": "zaentrum@example.org", "password": "password-for-tests"}
		got := refused(t, e.do(adminPortal, http.MethodPut, "/api/portal/mail", b), http.StatusUnprocessableEntity,
			"mail_write_refused", "The cluster refused the mail settings.")
		if !strings.Contains(got.Detail, `unknown field \"spec.mail.user\"`) && !strings.Contains(got.Detail, `unknown field "spec.mail.user"`) {
			t.Errorf("detail = %q", got.Detail)
		}
		if len(e.kube.SecretNames()) != 0 || e.mailSpec() != nil {
			t.Errorf("secrets %v, spec.mail %v", e.kube.SecretNames(), e.mailSpec())
		}
	})
}

// R.C2: a value of the wrong kind in any field of PUT /mail and POST
// /mail/test is refused with its field's sentence and code, as any other
// value, and no decoder's words reach the client; a port of 0 is no port,
// and one left out stays the encryption's default. A body that is no JSON
// object keeps the shared answer.
func TestRC2AValueOfTheWrongKind(t *testing.T) {
	put := func(field string, value any) map[string]any {
		b := map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org", "password": "password-for-tests"}
		b[field] = value
		return b
	}
	for _, c := range []struct {
		field     string
		values    []any
		code, say string
	}{
		{"port", []any{"587", 587.5, 0, -1, 70000, json.RawMessage("1e400"), true, map[string]any{}, []any{587}},
			"mail_port_invalid", "The port is a number from 1 to 65535."},
		{"host", []any{5, true, map[string]any{}, []any{"smtp.example.org"}}, "mail_server_invalid", "The server is not a host name or an IP address."},
		{"encryption", []any{1, false, []any{"tls"}}, "mail_encryption_invalid", "Encryption is starttls, tls or none."},
		{"from", []any{5, map[string]any{"address": "zaentrum@example.org"}}, "mail_sender_invalid", "The sender's address is not an e-mail address."},
		{"fromName", []any{5, []any{"Our server"}}, "mail_sender_name_too_long", "The sender's name has at most 60 characters."},
		{"user", []any{5, map[string]any{}}, "mail_value_invalid", "user has a value of the wrong kind."},
		{"password", []any{12345678, []any{"password-for-tests"}}, "mail_value_invalid", "password has a value of the wrong kind."},
		{"alertsTo", []any{map[string]any{}, "ops@example.org", []any{5}, []any{map[string]any{}}, 7},
			"mail_further_address_invalid", "One of the further addresses is not an e-mail address."},
	} {
		for _, v := range c.values {
			raw, _ := json.Marshal(v)
			t.Run(fmt.Sprintf("R.C2 %s %s", c.field, raw), func(t *testing.T) {
				e := newMailEnv(t, true, mailtest.Options{})
				e.putPlatform(nil, "")
				rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", put(c.field, v))
				got := refused(t, rec, http.StatusBadRequest, c.code, c.say)
				if c.code == "mail_value_invalid" && (len(got.Values) != 1 || got.Values["field"] != c.field) {
					t.Errorf("values = %v, want the field the sentence names", got.Values)
				}
				if body := rec.Body.String(); strings.Contains(body, "json") || strings.Contains(body, "unmarshal") || strings.Contains(body, "Go ") {
					t.Errorf("the decoder's words reached the client: %s", body)
				}
				if len(e.writes()) != 0 || len(e.kube.SecretNames()) != 0 {
					t.Errorf("a refused PUT wrote %v", e.writes())
				}
			})
		}
	}
	t.Run("R.C2 a port left out or null is the encryption's default", func(t *testing.T) {
		for _, b := range []map[string]any{
			{"host": "smtp.example.org", "from": "zaentrum@example.org", "encryption": "tls"},
			{"host": "smtp.example.org", "from": "zaentrum@example.org", "encryption": "tls", "port": nil},
		} {
			e := newMailEnv(t, true, mailtest.Options{})
			e.putPlatform(nil, "")
			if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", b); rec.Code != http.StatusOK {
				t.Fatalf("PUT %v = %d %s", b, rec.Code, rec.Body)
			}
			if _, has := e.mailSpec()["port"]; has || e.getMail().Port != 465 {
				t.Errorf("%v: spec.mail %v, GET port %d", b, e.mailSpec(), e.getMail().Port)
			}
		}
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, "")
		if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org", "port": 2525.0}); rec.Code != http.StatusOK ||
			e.mailSpec()["port"] != json.Number("2525") {
			t.Errorf("a whole number: %d %s, spec.mail %v", rec.Code, rec.Body, e.mailSpec())
		}
	})
	t.Run("R.C2 the test mail's to", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.None)
		for _, v := range []any{5, true, []any{"mia@example.org"}, map[string]any{"address": "mia@example.org"}} {
			rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", map[string]any{"to": v})
			refused(t, rec, http.StatusBadRequest, "address_invalid", "This is not an e-mail address.")
			if strings.Contains(rec.Body.String(), "json") {
				t.Errorf("the decoder's words reached the client: %s", rec.Body)
			}
		}
		if len(e.smtp.Sessions()) != 0 {
			t.Error("a connection was opened")
		}
	})
	t.Run("R.C2 a body that is no JSON object keeps the shared answer", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.ready(zmail.None)
		for _, route := range []string{"/api/portal/mail", "/api/portal/mail/test"} {
			method := http.MethodPut
			if strings.HasSuffix(route, "/test") {
				method = http.MethodPost
			}
			for _, raw := range []string{`[]`, `"mail"`, `5`, `not json`} {
				req := httptest.NewRequest(method, route, strings.NewReader(raw))
				req.Header.Set("Authorization", "Bearer "+e.iss.Token(t, adminPortal))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				e.h.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest || rec.Body.String() != "the body is not an object\n" {
					t.Errorf("%s %s %s = %d %s", method, route, raw, rec.Code, rec.Body)
				}
			}
		}
		if len(e.writes()) != 0 || len(e.smtp.Sessions()) != 0 {
			t.Errorf("writes %v", e.writes())
		}
	})
}

// R.C2: no setting takes a control character: portal-api refuses one, from a
// client of the API, in the server, the sender, the sender's name, the user,
// the password and every further address, before it writes anything. What is
// trimmed from around a setting is not in it.
func TestRC2NoControlCharacterInASetting(t *testing.T) {
	for _, c := range []struct {
		field string
		value any
	}{
		{"host", "smtp.example\x00.org"}, {"host", "smtp.example.org\x00"},
		{"from", "zaentrum@example.org\r\nBcc: tom@example.org"},
		{"fromName", "Our\nserver"}, {"fromName", "Our\u0085server"}, {"fromName", "Our\x7fserver"},
		{"user", "zaentrum\t@example.org"}, {"user", "zaentrum\x00"},
		{"password", "password\x00for-tests"}, {"password", "password-for-tests\n"},
		{"alertsTo", []any{"ops@example.org", "tom@example.org\x00"}},
		{"encryption", "tls\x00"},
	} {
		t.Run(fmt.Sprintf("R.C2 %s %q", c.field, c.value), func(t *testing.T) {
			e := newMailEnv(t, true, mailtest.Options{})
			e.putPlatform(nil, "")
			b := map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org", "user": "zaentrum@example.org", "password": "password-for-tests"}
			b[c.field] = c.value
			refused(t, e.do(adminPortal, http.MethodPut, "/api/portal/mail", b), http.StatusBadRequest,
				"mail_control_character", "The mail settings hold a character that cannot be used.")
			if len(e.writes()) != 0 || len(e.kube.SecretNames()) != 0 {
				t.Errorf("a refused PUT wrote %v", e.writes())
			}
		})
	}
	t.Run("R.C2 trimmed from around a setting", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, "")
		b := map[string]any{"host": "\tsmtp.example.org\r\n", "from": " zaentrum@example.org\n", "fromName": "Our server\n", "alertsTo": []any{"ops@example.org\n"}}
		if rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", b); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
		}
		if spec := e.mailSpec(); !jsonEqual(spec, map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org", "fromName": "Our server", "alertsTo": []any{"ops@example.org"}}) {
			t.Errorf("spec.mail = %v", spec)
		}
	})
}

// R.C2: an address, wherever one is typed, is one bare address whose domain
// is a host name of at least two parts: the sender, a further address and the
// test mail's to; one with accented or non-Latin letters is refused for its
// letters.
func TestRC2WhatAnAddressIs(t *testing.T) {
	for _, c := range []struct {
		field     string
		value     any
		code, say string
	}{
		{"from", "zaentrum@b", "mail_sender_invalid", "The sender's address is not an e-mail address."},
		{"from", "zaentrum@[127.0.0.1]", "mail_sender_invalid", "The sender's address is not an e-mail address."},
		{"from", "zaentrum@-example.org", "mail_sender_invalid", "The sender's address is not an e-mail address."},
		{"from", "zäntrum@example.org", "address_unsupported", "Addresses with accented or non-Latin letters are not supported."},
		{"alertsTo", []any{"ops@b"}, "mail_further_address_invalid", "One of the further addresses is not an e-mail address."},
		{"alertsTo", []any{"ops@[127.0.0.1]"}, "mail_further_address_invalid", "One of the further addresses is not an e-mail address."},
		{"alertsTo", []any{"ops@example.org", "ops@exämple.example.org"}, "address_unsupported", "Addresses with accented or non-Latin letters are not supported."},
	} {
		t.Run(fmt.Sprintf("R.C2 %s %v", c.field, c.value), func(t *testing.T) {
			e := newMailEnv(t, true, mailtest.Options{})
			e.putPlatform(nil, "")
			b := map[string]any{"host": "smtp.example.org", "from": "zaentrum@example.org"}
			b[c.field] = c.value
			refused(t, e.do(adminPortal, http.MethodPut, "/api/portal/mail", b), http.StatusBadRequest, c.code, c.say)
			if len(e.writes()) != 0 {
				t.Errorf("a refused PUT wrote %v", e.writes())
			}
		})
	}
	e := newMailEnv(t, true, mailtest.Options{})
	e.ready(zmail.None)
	for to, want := range map[string]string{
		"mia@b": "address_invalid", "mia@[127.0.0.1]": "address_invalid", "mia@-example.org": "address_invalid",
		"mïa@example.org": "address_unsupported", "мия@example.org": "address_unsupported",
	} {
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/mail/test", map[string]any{"to": to})
		say := "This is not an e-mail address."
		if want == "address_unsupported" {
			say = "Addresses with accented or non-Latin letters are not supported."
		}
		refused(t, rec, http.StatusBadRequest, want, say)
	}
	if len(e.smtp.Sessions()) != 0 {
		t.Error("a connection was opened")
	}
}

// R.C2: GET /mail says secretMissing where the condition Mail says the
// password's Secret is missing, and runByOperator, false where the operator
// does not run the platform.
func TestRC2GetMailSaysWhereMailStands(t *testing.T) {
	e := newMailEnv(t, true, mailtest.Options{})
	e.ready(zmail.StartTLS)
	if d := e.getMail(); d.SecretMissing || !d.RunByOperator {
		t.Errorf("ready: %+v", d)
	}
	e.setCondition(operator.MailSecretMissing, 1)
	if d := e.getMail(); !d.SecretMissing || !d.RunByOperator || d.Ready {
		t.Errorf("the Secret is missing: %+v", d)
	}
	// A condition of an older generation says nothing of this one.
	e.setCondition(operator.MailSecretMissing, 0)
	obj := e.kube.Object("zaentrums", "zaentrum")
	obj["metadata"].(map[string]any)["generation"] = 3
	e.kube.Put("zaentrums", obj)
	e.setCondition(operator.MailSecretMissing, 2)
	if d := e.getMail(); d.SecretMissing {
		t.Errorf("an older generation's condition: %+v", d)
	}
	other := newMailEnv(t, false, mailtest.Options{})
	other.api.cfg.Mail = other.settings(zmail.None, "", "")
	if d := other.getMail(); d.RunByOperator || d.SecretMissing || !d.Ready {
		t.Errorf("not run by the operator: %+v", d)
	}
}

// R.C2: a refusal's values are numbers and names as they are: a number goes
// as a JSON number, a name as a string.
func TestRC2ValuesAreNumbersAndNames(t *testing.T) {
	raw, err := json.Marshal(refusal{Error: "example_case", Values: map[string]any{"count": 10, "address": "mia@example.org"}, Message: "A sentence."})
	if err != nil || string(raw) != `{"error":"example_case","values":{"address":"mia@example.org","count":10},"message":"A sentence."}` {
		t.Errorf("refusal = %s, %v", raw, err)
	}
	got := failedSend(&zmail.Failure{Reason: zmail.RecipientRefused, Address: "mia@example.org"})
	if !reflect.DeepEqual(got.Values, map[string]any{"address": "mia@example.org"}) || got.Error != "mail_recipient_refused" {
		t.Errorf("a failed send = %+v", got)
	}
	if got := failedSend(&zmail.Failure{Reason: zmail.Unreachable}); got.Values != nil {
		t.Errorf("a failed send that names nothing = %+v", got)
	}
}
