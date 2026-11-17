package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth/authtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak/kctest"
	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// addAdmins seeds the realm with admins and others: bert, an admin with an
// address; carl, an admin whose account is disabled; dora, no admin; eve, an
// admin at the placeholder domain; and fred, an admin whose address is also a
// further one. Alert mail goes to owner, bert and the further address, each
// once.
func (e *alertEnv) addAdmins() {
	admin := []string{kctest.AdminRole, kctest.UserRole}
	e.kc.Add(kctest.Seed{Username: "bert", FirstName: "Bert", Email: "bert@example.org", Roles: admin})
	e.kc.Add(kctest.Seed{Username: "carl", FirstName: "Carl", Email: "carl@example.org", Disabled: true, Roles: admin})
	e.kc.Add(kctest.Seed{Username: "dora", FirstName: "Dora", Email: "dora@example.org", Roles: []string{kctest.UserRole}})
	e.kc.Add(kctest.Seed{Username: "eve", FirstName: "Eve", Email: "eve@zaentrum.local", Roles: admin})
	e.kc.Add(kctest.Seed{Username: "fred", FirstName: "Fred", Email: "ops@example.org", Roles: admin})
}

// mailed are the addresses each mail the server took went to, in order, and
// the mails.
func (e *alertEnv) mailed() ([]string, []mailtest.Mail) {
	var to []string
	mails := e.smtp.Mails()
	for _, m := range mails {
		to = append(to, m.To...)
	}
	return to, mails
}

// mailOf is what GET /alerts says of the mail of the alert of kind.
func (e *alertEnv) mailOf(kind string) (string, int, *int, *refusal) {
	e.t.Helper()
	for _, a := range e.get().Alerts {
		if a.Kind == kind {
			return a.Mail.State, a.Mail.Addresses, a.Mail.Taken, a.Mail.Error
		}
	}
	e.t.Fatalf("no alert of %s", kind)
	return "", 0, nil, nil
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// R.C8: an alert's start is mailed to every admin whose account is enabled
// and has an address, and to every further address, each once — the
// addresses alerts go to — with its title and text as they were then, and its
// end to the addresses the start reached, with its last title; each mail as
// the Words have it.
func TestRC8AnAlertIsMailedWhenItStartsAndWhenItEnds(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.addAdmins()
	logs := captureLog(t)
	e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
	e.set("certificate", map[string]any{"notAfter": "2026-11-30T00:00:00Z", "names": []any{"zaentrum.example.org"}})
	e.check()
	to, mails := e.mailed()
	if want := []string{"bert@example.org", "ops@example.org", "owner@example.org"}; !slices.Equal(sorted(to), want) || len(mails) != 3 {
		t.Fatalf("mailed to %v, want %v", to, want)
	}
	subject, header, text := readMail(t, mails[0])
	if subject != "The certificate ends in 14 days" || header.Get("Auto-Submitted") != "auto-generated" ||
		text != "The platform's certificate ends on 30 November 2026, 00:00 UTC. After that browsers warn and the apps stop connecting. Renew it.\n\n"+
			"See it: https://zaentrum.example.org/portal/alerts\n\n"+
			"You get this mail because you are an admin of zaentrum.example.org, or your address is in its alert settings.\n" {
		t.Fatalf("the start mail: %q\n%q", subject, text)
	}
	if state, n, _, _ := e.mailOf(kindCertificate); state != mailStateSent || n != 3 {
		t.Fatalf("mail %s %d", state, n)
	}
	// Its title changes a day later; the end mail's subject is the last.
	e.later(24 * time.Hour)
	e.check()
	if _, mails := e.mailed(); len(mails) != 3 {
		t.Fatalf("mailed again as its figures changed: %d", len(mails))
	}
	e.set("certificate", map[string]any{"notAfter": "2027-03-01T00:00:00Z", "names": []any{"zaentrum.example.org"}})
	e.check()
	to, mails = e.mailed()
	if len(mails) != 6 || !slices.Equal(sorted(to[3:]), []string{"bert@example.org", "ops@example.org", "owner@example.org"}) {
		t.Fatalf("the end mailed to %v", to)
	}
	subject, _, text = readMail(t, mails[5])
	if subject != "Ended: The certificate ends in 13 days" ||
		text != "The certificate now ends on 1 March 2027, 00:00 UTC.\n\n"+
			"It started on 16 November 2026, 12:00 UTC and ended on 17 November 2026, 12:00 UTC.\n\n"+
			"You get this mail because you are an admin of zaentrum.example.org, or your address is in its alert settings.\n" {
		t.Fatalf("the end mail: %q\n%q", subject, text)
	}
	if state, n, _, _ := e.mailOf(kindCertificate); state != mailStateSent || n != 3 {
		t.Fatalf("after the end, mail %s %d", state, n)
	}
	for _, want := range []string{"alerts: certificate-ends about certificate started", "mail: an alert was sent", "alerts: certificate-ends about certificate ended", "mail: an alert's end was sent"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs.String(), "@") || strings.Contains(logs.String(), "browsers warn") {
		t.Errorf("an address or a text in the log:\n%s", logs)
	}
}

// R.C8: nothing is mailed while mail is not set up, while alert mail is off,
// or where nobody has an address; the mail says which, also once that changes
// while the alert is active, and the end is not mailed either.
func TestRC8WhereNoAlertIsMailed(t *testing.T) {
	for _, c := range []struct {
		name, want      string
		bundled         bool
		change, change2 func(e *alertEnv)
	}{
		{"not set up", mailStateNotSetUp, true,
			func(e *alertEnv) { delete(e.spec, "mail") },
			func(e *alertEnv) { e.spec["mail"] = specOf(e.api.cfg.Mail, "") }},
		{"alert mail is off", mailStateOff, true,
			func(e *alertEnv) { e.spec["alerts"] = map[string]any{"mail": false} },
			func(e *alertEnv) { delete(e.spec, "alerts") }},
		{"nobody has an address", mailStateNoAddress, false,
			func(e *alertEnv) {
				e.api.cfg.Mail.AlertsTo = nil
				delete(e.spec["mail"].(map[string]any), "alertsTo")
			},
			func(e *alertEnv) {
				e.api.cfg.Mail.AlertsTo = []string{"ops@example.org"}
				e.spec["mail"].(map[string]any)["alertsTo"] = []string{"ops@example.org"}
			}},
	} {
		t.Run("R.C8 "+c.name, func(t *testing.T) {
			e := newAlertEnv(t, mailtest.Options{})
			if !c.bundled {
				e.api.people = nil
			}
			c.change(e)
			e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
			e.check()
			if state, n, _, _ := e.mailOf(kindPart); state != c.want || n != 0 {
				t.Fatalf("mail %s %d, want %s", state, n, c.want)
			}
			c.change2(e)
			e.later(5 * time.Minute)
			e.put()
			e.check()
			if state, _, _, _ := e.mailOf(kindPart); state != c.want {
				t.Fatalf("once mail can go, mail %s", state)
			}
			e.set("components", []any{part("katalog-api", true, time.Time{})})
			e.check()
			if state, _, _, _ := e.mailOf(kindPart); state != c.want {
				t.Fatalf("after the end, mail %s", state)
			}
			if _, mails := e.mailed(); len(mails) != 0 {
				t.Fatalf("%d mails", len(mails))
			}
		})
	}
}

// R.C8: an alert that starts while alert mail is off says so, though one was
// sent less than a day ago too: the setting is what holds its mail back.
func TestRC8AlertMailOffIsSaidFirst(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.check()
	e.later(time.Hour)
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	e.check()
	e.spec["alerts"] = map[string]any{"mail": false}
	e.later(time.Hour)
	e.set("components", []any{part("katalog-api", false, e.clock.Add(-time.Hour))})
	e.check()
	var states []string
	for _, a := range e.get().Alerts {
		states = append(states, a.Mail.State)
	}
	if !slices.Equal(states, []string{mailStateOff, mailStateSent}) {
		t.Fatalf("mail %v, want off for the one that started while alert mail is off", states)
	}
}

// R.C8: for the same alert about the same subject one start mail is sent in
// 24 hours, counted from the last start whose mail the server took: one that
// starts again sooner is not mailed, and neither is its end.
func TestRC8OneStartMailIn24Hours(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	logs := captureLog(t)
	notReady := []any{part("katalog-api", false, alertNow.Add(-time.Hour))}
	ready := []any{part("katalog-api", true, time.Time{})}
	e.set("components", notReady)
	e.check()
	e.later(time.Hour)
	e.set("components", ready)
	e.check()
	if _, mails := e.mailed(); len(mails) != 4 { // owner and ops, start and end
		t.Fatalf("%d mails, want 4", len(mails))
	}
	e.later(time.Hour)
	e.set("components", []any{part("katalog-api", false, e.clock.Add(-time.Hour))})
	e.check()
	if state, _, _, _ := e.mailOf(kindPart); state != mailStateSentRecently {
		t.Fatalf("started again sooner: %s", state)
	}
	e.later(time.Hour)
	e.set("components", ready)
	e.check()
	if _, mails := e.mailed(); len(mails) != 4 {
		t.Fatalf("%d mails after it started again and ended", len(mails))
	}
	if !strings.Contains(logs.String(), "mail: an alert was not sent: one was sent less than a day ago") {
		t.Errorf("the log:\n%s", logs)
	}
	// 24 hours after the first start's mail — not less than a day ago:
	// mailed again.
	e.later(21 * time.Hour)
	e.set("components", []any{part("katalog-api", false, e.clock.Add(-time.Hour))})
	e.check()
	if _, mails := e.mailed(); len(mails) != 6 {
		t.Fatalf("%d mails a day later, want 6", len(mails))
	}
}

// recipients are the RCPT commands each session sent, by address.
func (e *alertEnv) tries() map[string]int {
	out := map[string]int{}
	for _, s := range e.smtp.Sessions() {
		for _, c := range s {
			if c.Verb == "RCPT" {
				out[strings.Trim(strings.TrimPrefix(c.Arg, "TO:"), "<>")]++
			}
		}
	}
	return out
}

// R.C8: a mail the server does not take is tried again after 5 minutes,
// three times in all for each address; so is one held back while mail is set
// up and not ready; and an alert that ends while its start mail still waits
// sends neither.
func TestRC8AMailIsTriedAgain(t *testing.T) {
	t.Run("R.C8 three times in all, then given up", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{Replies: map[string]string{"RCPT": "550 5.1.1 no such user"}})
		logs := captureLog(t)
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		if state, n, _, _ := e.mailOf(kindPart); state != mailStateSending || n != 2 {
			t.Fatalf("after the first try: %s %d", state, n)
		}
		e.later(4 * time.Minute)
		e.check()
		if got := e.tries(); got["ops@example.org"] != 1 {
			t.Fatalf("tried again before 5 minutes: %v", got)
		}
		for i := 0; i < 4; i++ {
			e.later(5 * time.Minute)
			e.check()
		}
		if got := e.tries(); got["ops@example.org"] != 3 || got["owner@example.org"] != 3 {
			t.Fatalf("tries %v, want 3 each", got)
		}
		state, n, taken, why := e.mailOf(kindPart)
		if state != mailStateFailed || n != 2 || taken != nil || why == nil || why.Error != "mail_recipient_refused" ||
			!strings.HasPrefix(why.Message, "The mail server refused the address ") {
			t.Fatalf("given up: %s %d %v %+v", state, n, taken, why)
		}
		for _, want := range []string{"mail: an alert was refused (recipient); it is tried again in 5 minutes", "mail: an alert was given up after 3 tries (recipient)"} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("the log lacks %q", want)
			}
		}
	})
	t.Run("R.C8 one address taken, another not", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.kc.Add(kctest.Seed{Username: "joerg", FirstName: "Jörg", Email: "jörg@example.org", Roles: []string{kctest.AdminRole, kctest.UserRole}})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		for i := 0; i < 3; i++ {
			e.check()
			e.later(5 * time.Minute)
		}
		state, n, taken, why := e.mailOf(kindPart)
		if state != mailStateFailed || n != 3 || taken == nil || *taken != 2 || why == nil || why.Error != "mail_recipient_refused" || why.Values["address"] != "jörg@example.org" {
			t.Fatalf("%s %d %v %+v", state, n, taken, why)
		}
		// The end goes to the addresses the start reached.
		e.set("components", []any{part("katalog-api", true, time.Time{})})
		e.check()
		to, _ := e.mailed()
		if !slices.Equal(sorted(to), []string{"ops@example.org", "ops@example.org", "owner@example.org", "owner@example.org"}) {
			t.Fatalf("mailed to %v", to)
		}
		if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 2 {
			t.Fatalf("the end: %s %d", state, n)
		}
	})
	t.Run("R.C8 held back while mail is not ready", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.api.cfg.Mail.FromName = "Another name" // the settings are still being applied
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.later(5 * time.Minute)
		e.check()
		if state, _, _, _ := e.mailOf(kindPart); state != mailStateSending || len(e.smtp.Sessions()) != 0 {
			t.Fatalf("while not ready: %s, %d sessions", state, len(e.smtp.Sessions()))
		}
		e.api.cfg.Mail.FromName = "Our server"
		e.later(5 * time.Minute)
		e.check()
		if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 2 {
			t.Fatalf("once ready, at its third try: %s %d", state, n)
		}
		e2 := newAlertEnv(t, mailtest.Options{})
		e2.api.cfg.Mail.FromName = "Another name"
		e2.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		for i := 0; i < 4; i++ {
			e2.check()
			e2.later(5 * time.Minute)
		}
		if state, _, _, why := e2.mailOf(kindPart); state != mailStateFailed || why == nil || why.Error != "mail_not_ready" ||
			why.Message != "The mail settings are still being applied." {
			t.Fatalf("never ready: %s %+v", state, why)
		}
	})
	t.Run("R.C8 a start mail tried again carries the title as it was when the alert started", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.api.cfg.Mail.FromName = "Another name" // not ready at the start
		e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
		// 13 days and 2 minutes: 14 days rounded up; 5 minutes later, 13.
		e.set("certificate", map[string]any{"notAfter": alertNow.Add(13*24*time.Hour + 2*time.Minute).Format(time.RFC3339)})
		e.check()
		e.api.cfg.Mail.FromName = "Our server"
		e.later(5 * time.Minute)
		e.check()
		if a := e.only(kindCertificate); a.Title != "The certificate ends in 13 days" {
			t.Fatalf("the title now %q", a.Title)
		}
		_, mails := e.mailed()
		if len(mails) != 2 {
			t.Fatalf("%d mails", len(mails))
		}
		if subject, _, _ := readMail(t, mails[0]); subject != "The certificate ends in 14 days" {
			t.Fatalf("the start mail's subject %q", subject)
		}
	})
	t.Run("R.C8 an alert that ends while its start mail waits sends neither", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{Replies: map[string]string{"RCPT": "451 4.3.0 try later"}})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.later(2 * time.Minute)
		e.set("components", []any{part("katalog-api", true, time.Time{})})
		e.check()
		for i := 0; i < 3; i++ {
			e.later(5 * time.Minute)
			e.check()
		}
		if got := e.tries(); got["ops@example.org"] != 1 || got["owner@example.org"] != 1 {
			t.Fatalf("tries %v: the start went on after its end", got)
		}
		if state, _, _, why := e.mailOf(kindPart); state != mailStateFailed || why == nil || why.Error != "mail_server_answered" {
			t.Fatalf("%s %+v", state, why)
		}
	})
}

// R.C8: where the admins cannot be read when an alert starts, the further
// addresses get the mail, and the admins' are tried again.
func TestRC8TheAdminsThatCannotBeRead(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	logs := captureLog(t)
	e.kc.Fail = http.StatusServiceUnavailable
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.check()
	if to, _ := e.mailed(); !slices.Equal(to, []string{"ops@example.org"}) {
		t.Fatalf("mailed to %v", to)
	}
	if state, _, _, _ := e.mailOf(kindPart); state != mailStateSending {
		t.Fatalf("mail %s", state)
	}
	if !strings.Contains(logs.String(), "mail: the admins could not be read, so an alert went to the further addresses alone: ") {
		t.Errorf("the log:\n%s", logs)
	}
	e.kc.Fail = 0
	e.later(5 * time.Minute)
	e.check()
	if to, _ := e.mailed(); !slices.Equal(to, []string{"ops@example.org", "owner@example.org"}) {
		t.Fatalf("mailed to %v", to)
	}
	if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 2 {
		t.Fatalf("mail %s %d", state, n)
	}
	// Never read, with no further address: nobody has one.
	e2 := newAlertEnv(t, mailtest.Options{})
	e2.api.cfg.Mail.AlertsTo = nil
	delete(e2.spec["mail"].(map[string]any), "alertsTo")
	e2.kc.Fail = http.StatusServiceUnavailable
	e2.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	for i := 0; i < 4; i++ {
		e2.check()
		e2.later(5 * time.Minute)
	}
	e2.kc.Fail = 0
	if state, _, _, _ := e2.mailOf(kindPart); state != mailStateNoAddress {
		t.Fatalf("mail %s", state)
	}
}

// secondAPI is another portal-api of the same platform: the same resource,
// people, mail server and alerts table.
func (e *alertEnv) secondAPI() *API {
	return &API{cfg: e.api.cfg, op: e.api.op, people: e.api.people, alerts: e.api.alerts, now: e.api.now}
}

// R.C8: where portal-api runs more than once, the write that starts an alert,
// and the one that ends it, decide which of them mails it: each address gets
// one start mail and one end mail.
func TestRC8TwoPortalAPIsMailAnAlertOnce(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	other := e.secondAPI()
	both := func() {
		var wg sync.WaitGroup
		for _, a := range []*API{e.api, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.checkAlerts(context.Background())
			}()
		}
		wg.Wait()
	}
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	both()
	e.later(time.Minute)
	both()
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	both()
	both()
	to, _ := e.mailed()
	if !slices.Equal(sorted(to), []string{"ops@example.org", "ops@example.org", "owner@example.org", "owner@example.org"}) {
		t.Fatalf("mailed to %v: want one start and one end each", to)
	}
	if n := len(e.alerts.all()); n != 1 {
		t.Fatalf("%d alerts", n)
	}
}

// R.C8: an admin has seen an alert when they have seen it since it last
// changed; unseen counts what the bell shows; an alert there is not is 404.
func TestRC8AlertsAreSeen(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	bert := authtest.Person("zaentrum-web", "bert", "zaentrum-admin", "zaentrum-user")
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour)), part("chino-api", false, alertNow.Add(-time.Hour))})
	e.check()
	got := e.get()
	if got.Unseen != 2 || len(got.Alerts) != 2 {
		t.Fatalf("%+v", got)
	}
	id := got.Alerts[0].ID
	rec := e.do(adminPortal, http.MethodPost, "/api/portal/alerts/"+id+"/read", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"unseen":1}` {
		t.Fatalf("read = %d %s", rec.Code, rec.Body)
	}
	got = e.get()
	if got.Unseen != 1 || !got.Alerts[0].Seen || got.Alerts[1].Seen {
		t.Fatalf("after one: %+v", got)
	}
	rec = e.do(bert, http.MethodGet, "/api/portal/alerts", nil)
	if b := decodeAs[alertsGot](t, rec); rec.Code != http.StatusOK || b.Unseen != 2 {
		t.Fatalf("another admin's: %d %s", rec.Code, rec.Body)
	}
	// It ends: it changed, and is unseen again.
	var ended model.Alert
	for _, a := range e.alerts.all() {
		if a.ID == id {
			ended = a
		}
	}
	e.later(time.Minute)
	e.set("components", []any{part(ended.About, true, time.Time{}), part(map[string]string{"katalog-api": "chino-api", "chino-api": "katalog-api"}[ended.About], false, alertNow.Add(-time.Hour))})
	e.check()
	if got = e.get(); got.Unseen != 2 {
		t.Fatalf("after its end: unseen %d", got.Unseen)
	}
	rec = e.do(adminPortal, http.MethodPost, "/api/portal/alerts/read-all", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"seen":2,"unseen":0}` {
		t.Fatalf("read-all = %d %s", rec.Code, rec.Body)
	}
	// Ended more than 7 days ago, it is listed and the bell does not count it.
	e.later(8 * 24 * time.Hour)
	if got = e.get(); got.Unseen != 0 || len(got.Alerts) < 1 {
		t.Fatalf("a week later: %+v", got)
	}
	for _, id := range []string{"00000000-0000-4000-8000-000000000099", "not-an-id"} {
		if rec := e.do(adminPortal, http.MethodPost, "/api/portal/alerts/"+id+"/read", nil); rec.Code != http.StatusNotFound {
			t.Errorf("read %s = %d %s", id, rec.Code, rec.Body)
		}
	}
}

// R.C8: an alert is kept until 90 days after it ended: the checks sweep the
// older ones, once an hour.
func TestRC8AlertsAreKeptUntil90DaysAfterTheyEnded(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	logs := captureLog(t)
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.check()
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	e.later(time.Minute)
	e.check()
	e.later(90*24*time.Hour - time.Minute)
	e.check()
	if n := len(e.of(kindPart)); n != 1 {
		t.Fatalf("swept before 90 days: %d", n)
	}
	// The sweep runs once an hour.
	e.later(time.Hour)
	e.check()
	if n := len(e.of(kindPart)); n != 0 {
		t.Fatalf("kept after 90 days: %d", n)
	}
	if !strings.Contains(logs.String(), "alerts: 1 that ended more than 90 days ago went") {
		t.Errorf("the log:\n%s", logs)
	}
}

// R.C8: an address at the placeholder domain is no address: alert mail does
// not go to a further one.
func TestRC8APlaceholderIsNoAddress(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	further := []string{"ops@example.org", "alerts@zaentrum.local", "Night@ZAENTRUM.LOCAL "}
	e.api.cfg.Mail.AlertsTo = further
	e.spec["mail"].(map[string]any)["alertsTo"] = further
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.check()
	if to, _ := e.mailed(); !slices.Equal(sorted(to), []string{"ops@example.org", "owner@example.org"}) {
		t.Fatalf("mailed to %v", to)
	}
	for address := range e.tries() {
		if strings.Contains(strings.ToLower(address), "zaentrum.local") {
			t.Errorf("tried %s", address)
		}
	}
	if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 2 {
		t.Fatalf("mail %s %d", state, n)
	}
}

// R.C8: the mail column never says the opposite of what happened: once the
// server took a mail for an address it is sent, to so many of so many,
// whatever changes afterwards; while some addresses have it and others wait,
// it is sending, with how many took it; where alert mail is switched off
// before the others took it, it went no further, and says why not to them.
func TestRC8TheMailSaysWhatHappened(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.kc.Add(kctest.Seed{Username: "joerg", FirstName: "Jörg", Email: "jörg@example.org", Roles: []string{kctest.AdminRole, kctest.UserRole}})
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.check()
	if state, n, taken, why := e.mailOf(kindPart); state != mailStateSending || n != 3 || taken == nil || *taken != 2 || why != nil {
		t.Fatalf("some have it, another waits: %s %d %v %+v", state, n, taken, why)
	}
	// It is sent to two of three whatever happens to mail afterwards.
	e.spec["alerts"] = map[string]any{"mail": false}
	e.later(5 * time.Minute)
	e.put()
	e.check()
	state, n, taken, why := e.mailOf(kindPart)
	if state != mailStateFailed || n != 3 || taken == nil || *taken != 2 || why == nil || why.Error != "mail_recipient_refused" {
		t.Fatalf("alert mail off: %s %d %v %+v", state, n, taken, why)
	}
	// Not later either.
	delete(e.spec, "alerts")
	for i := 0; i < 2; i++ {
		e.later(5 * time.Minute)
		e.put()
		e.check()
	}
	for _, to := range e.startMail(kindPart).To {
		if !to.Sent && to.Tries != 1 {
			t.Fatalf("tried again once alert mail was on again: %+v", to)
		}
	}
	delete(e.spec, "mail")
	e.put()
	if state, n, taken, _ := e.mailOf(kindPart); state != mailStateFailed || n != 3 || *taken != 2 {
		t.Fatalf("mail no longer set up: %s %d %v", state, n, taken)
	}
}

// R.C8: a mail that meets mail not set up, or alert mail off, at a try again
// goes no further: nothing is sent, not later either, and the column says
// which, never that it is tried again; nor is the end mailed.
func TestRC8AMailGoesNoFurtherWhereNoMailCanGo(t *testing.T) {
	for _, c := range []struct {
		name, state string
		stop, again func(e *alertEnv)
	}{
		{"mail is not set up", mailStateNotSetUp,
			func(e *alertEnv) { delete(e.spec, "mail") },
			func(e *alertEnv) { e.spec["mail"] = specOf(e.api.cfg.Mail, "") }},
		{"alert mail is off", mailStateOff,
			func(e *alertEnv) { e.spec["alerts"] = map[string]any{"mail": false} },
			func(e *alertEnv) { delete(e.spec, "alerts") }},
	} {
		t.Run("R.C8 "+c.name, func(t *testing.T) {
			e := newAlertEnv(t, mailtest.Options{})
			e.api.cfg.Mail.FromName = "Another name" // the settings are still being applied
			e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
			e.check()
			e.api.cfg.Mail.FromName = "Our server"
			c.stop(e)
			for i := 0; i < 3; i++ {
				e.later(5 * time.Minute)
				e.put()
				e.check()
			}
			if n := len(e.smtp.Sessions()); n != 0 {
				t.Fatalf("%d sessions while %s", n, c.name)
			}
			if state, _, _, _ := e.mailOf(kindPart); state != c.state {
				t.Fatalf("mail %s, want %s", state, c.state)
			}
			c.again(e)
			for i := 0; i < 3; i++ {
				e.later(5 * time.Minute)
				e.put()
				e.check()
			}
			if state, _, _, _ := e.mailOf(kindPart); state != c.state || len(e.smtp.Sessions()) != 0 {
				t.Fatalf("once mail can go again: %s, %d sessions", state, len(e.smtp.Sessions()))
			}
			e.set("components", []any{part("katalog-api", true, time.Time{})})
			e.check()
			if state, _, _, _ := e.mailOf(kindPart); state != c.state || len(e.smtp.Sessions()) != 0 {
				t.Fatalf("after the end: %s, %d sessions", state, len(e.smtp.Sessions()))
			}
		})
	}
}

// R.C8: mail that is set up and not ready — the settings still being
// applied, the password missing — holds a mail back: each check that finds it
// so is a try, 5 minutes apart, three in all, the mail not sent yet and tried
// again within 5 minutes; after the third it could not be sent, with the
// reason.
func TestRC8MailSetUpAndNotReadyIsTriedThreeTimes(t *testing.T) {
	for _, c := range []struct {
		name, code, message string
		notReady            func(e *alertEnv)
	}{
		{"the settings still being applied", "mail_not_ready", "The mail settings are still being applied.",
			func(e *alertEnv) { e.api.cfg.Mail.FromName = "Another name" }},
		{"the password missing", "mail_password_missing", "The mail server's password is missing.",
			func(e *alertEnv) {
				e.status["conditions"] = []any{cond("Mail", "False", operator.MailSecretMissing, "there is no Secret zaentrum-mail")}
			}},
	} {
		t.Run("R.C8 "+c.name, func(t *testing.T) {
			e := newAlertEnv(t, mailtest.Options{})
			c.notReady(e)
			e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
			tries := func() []int {
				var out []int
				for _, to := range e.startMail(kindPart).To {
					out = append(out, to.Tries)
				}
				return out
			}
			for i, want := range []int{1, 1, 2, 3, 3} {
				e.check()
				if got := tries(); !slices.Equal(got, []int{want, want}) {
					t.Fatalf("check %d: tries %v, want %d each", i, got, want)
				}
				state, n, _, why := e.mailOf(kindPart)
				if want < 3 && (state != mailStateSending || n != 2 || why != nil) {
					t.Fatalf("check %d: %s %d %+v, want not sent yet", i, state, n, why)
				}
				if want == 3 && (state != mailStateFailed || why == nil || why.Error != c.code || why.Message != c.message) {
					t.Fatalf("check %d: %s %+v, want could not be sent", i, state, why)
				}
				if i == 0 {
					e.later(4 * time.Minute)
				} else {
					e.later(5 * time.Minute)
				}
			}
			if len(e.smtp.Sessions()) != 0 {
				t.Errorf("%d sessions while mail was not ready", len(e.smtp.Sessions()))
			}
		})
	}
}

// failMailReads has the read of the platform's resource that follows each
// hold of an alert's mail refused: a check reads the facts, and its mails
// cannot read their settings.
func (e *alertEnv) failMailReads() {
	var next atomic.Bool
	e.alerts.fail = func(method string, a model.Alert) error {
		if method == "LeaseMail" {
			next.Store(true)
		}
		return nil
	}
	e.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
		if c.Method == http.MethodGet && strings.HasSuffix(c.Path, "/zaentrums") && next.CompareAndSwap(true, false) {
			return http.StatusForbidden, `zaentrums.zaentrum.io is forbidden: User "system:serviceaccount:zaentrum:portal-api" cannot list resource "zaentrums"`, true
		}
		return 0, "", false
	}
}

// startMail is the start mail as the alerts table keeps it.
func (e *alertEnv) startMail(kind string) mailRound {
	e.t.Helper()
	var m alertMail
	if err := json.Unmarshal(e.only(kind).Mail, &m); err != nil {
		e.t.Fatal(err)
	}
	return m.Start
}

// R.C8: mail settings that cannot be read count as a try of each mail that
// waits, with the cluster's refusal as its reason, and say so in the log; the
// next is made after 5 minutes.
func TestRC8MailSettingsThatCannotBeReadCountAsATry(t *testing.T) {
	t.Run("R.C8 a mail that has not begun", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		logs := captureLog(t)
		e.failMailReads()
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		if r := e.startMail(kindPart); r.Begun || r.Tries != 1 || r.Error == nil || r.Error.Error != "platform_read_forbidden" {
			t.Fatalf("after the first try %+v", r)
		}
		e.later(4 * time.Minute)
		e.check()
		if r := e.startMail(kindPart); r.Tries != 1 {
			t.Fatalf("tried again within 5 minutes: %d", r.Tries)
		}
		for i := 0; i < 3; i++ {
			e.later(5 * time.Minute)
			e.check()
		}
		e.alerts.fail, e.kube.Fail = nil, nil
		state, _, _, why := e.mailOf(kindPart)
		if state != mailStateFailed || why == nil || why.Error != "platform_read_forbidden" || why.Detail == "" {
			t.Fatalf("given up: %s %+v", state, why)
		}
		if len(e.smtp.Sessions()) != 0 {
			t.Error("a mail was sent")
		}
		for _, want := range []string{
			"alerts: the platform's resource could not be read: ",
			"mail: an alert was refused (platform_read_forbidden); it is tried again in 5 minutes",
			"mail: an alert was given up after 3 tries (platform_read_forbidden)",
		} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("the log lacks %q:\n%s", want, logs)
			}
		}
	})
	t.Run("R.C8 a mail some addresses wait for", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.api.cfg.Mail.FromName = "Another name" // not ready: a try each
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.api.cfg.Mail.FromName = "Our server"
		e.failMailReads()
		e.later(5 * time.Minute)
		e.check()
		r := e.startMail(kindPart)
		for _, to := range r.To {
			if to.Tries != 2 || to.Error == nil || to.Error.Error != "platform_read_forbidden" {
				t.Errorf("%+v", to)
			}
		}
		e.alerts.fail, e.kube.Fail = nil, nil
		e.later(5 * time.Minute)
		e.check()
		if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 2 {
			t.Fatalf("at its third try: %s %d", state, n)
		}
	})
}

// R.C8: the addresses an alert was mailed to are kept until its end mail is
// done, and then their number, how many took it, and one reason with no
// address in it.
func TestRC8TheAddressesAreKeptUntilTheEndMailIsDone(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.kc.Add(kctest.Seed{Username: "joerg", FirstName: "Jörg", Email: "jörg@example.org", Roles: []string{kctest.AdminRole, kctest.UserRole}})
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	for i := 0; i < 3; i++ {
		e.check()
		e.later(5 * time.Minute)
	}
	if raw := string(e.only(kindPart).Mail); !strings.Contains(raw, "owner@example.org") {
		t.Fatalf("while it is active the addresses are kept: %s", raw)
	}
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	e.check()
	raw := string(e.only(kindPart).Mail)
	if strings.Contains(raw, "@") {
		t.Fatalf("an address is kept after the end mail: %s", raw)
	}
	var m alertMail
	_ = json.Unmarshal([]byte(raw), &m)
	if s := m.Start; !s.Done || s.Addresses != 3 || s.Taken != 2 || s.Reason == nil || s.Reason.Error != "mail_recipient_refused" ||
		s.Reason.Message != "The mail server refused an address." || len(s.Reason.Values) != 0 {
		t.Fatalf("the start mail %+v %+v", s, s.Reason)
	}
	if end := m.End; end == nil || !end.Done || end.Addresses != 2 || end.Taken != 2 {
		t.Fatalf("the end mail %+v", end)
	}
	if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 2 {
		t.Fatalf("mail %s %d", state, n)
	}
	// An alert whose start reached nobody keeps its number and its reason.
	e2 := newAlertEnv(t, mailtest.Options{Replies: map[string]string{"RCPT": "550 5.1.1 <owner@example.org>: no such user"}})
	e2.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e2.check()
	e2.set("components", []any{part("katalog-api", true, time.Time{})})
	e2.later(time.Minute)
	e2.check()
	raw = string(e2.only(kindPart).Mail)
	if strings.Contains(raw, "@") {
		t.Fatalf("an address is kept after the end: %s", raw)
	}
	if state, n, _, why := e2.mailOf(kindPart); state != mailStateFailed || n != 2 || why == nil || why.Error != "mail_recipient_refused" ||
		why.Message != "The mail server refused an address." || len(why.Values) != 0 {
		t.Fatalf("mail %s %d %+v", state, n, why)
	}
	// A refusal in the server's own words that named an address says the
	// same once the address is no longer kept.
	e3 := newAlertEnv(t, mailtest.Options{Replies: map[string]string{".": "554 5.7.1 <owner@example.org>: relay access denied"}})
	e3.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e3.check()
	if _, _, _, why := e3.mailOf(kindPart); why != nil {
		t.Fatalf("a reason before the third try: %+v", why)
	}
	e3.set("components", []any{part("katalog-api", true, time.Time{})})
	e3.later(time.Minute)
	e3.check()
	if raw := string(e3.only(kindPart).Mail); strings.Contains(raw, "@") {
		t.Fatalf("an address is kept after the end: %s", raw)
	}
	if state, _, _, why := e3.mailOf(kindPart); state != mailStateFailed || why == nil || why.Error != "mail_recipient_refused" ||
		why.Message != "The mail server refused an address." || len(why.Values) != 0 {
		t.Fatalf("mail %s %+v", state, why)
	}
}
