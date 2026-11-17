package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	zmail "github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// eventually waits up to 5 seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("not within 5 seconds: %s", what)
}

// sendThrough is a's own way of sending a mail, through the test server.
func sendThrough(a *API) func(ctx context.Context, m zmail.Message) error {
	return func(ctx context.Context, m zmail.Message) error {
		return zmail.New(a.cfg.Mail, a.mailLocalName()).Send(ctx, m)
	}
}

// R.C8: RunAlerts checks at once and then every 60 seconds until it is told
// to stop; it raises nothing where the operator does not run the platform.
func TestRC8RunAlerts(t *testing.T) {
	t.Run("R.C8 a check at once, one at each tick, and none once it is told to stop", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		logs := captureLog(t)
		ticks, every, stopped := make(chan time.Time), make(chan time.Duration, 1), make(chan struct{})
		e.api.alertLoop.ticker = func(d time.Duration) (<-chan time.Time, func()) {
			every <- d
			return ticks, func() { close(stopped) }
		}
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.api.RunAlerts(ctx)
		}()
		eventually(t, "the check at once", func() bool { return len(e.alerts.all()) == 1 })
		if d := <-every; d != 60*time.Second {
			t.Fatalf("ticks every %s", d)
		}
		e.set("components", []any{part("katalog-api", true, time.Time{})})
		ticks <- alertNow
		eventually(t, "the check at the tick", func() bool { return e.alerts.all()[0].EndedAt != nil })
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("it did not stop")
		}
		<-stopped
		if !strings.Contains(logs.String(), "alerts: the platform's status is read every 60s; an alert is kept 90 days after it ends\n") {
			t.Errorf("the log:\n%s", logs)
		}
	})
	t.Run("R.C8 nothing where the operator does not run the platform", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		logs := captureLog(t)
		e.api.op = nil
		asked := false
		e.api.alertLoop.ticker = func(time.Duration) (<-chan time.Time, func()) {
			asked = true
			return nil, func() {}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.api.RunAlerts(context.Background())
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("it runs")
		}
		if asked || !strings.Contains(logs.String(), "alerts: the platform is not run by the operator, so no alert is raised\n") {
			t.Errorf("ticker asked %v, the log:\n%s", asked, logs)
		}
	})
}

// R.C8: a check that fails is said in the log, and the next one runs; a mail
// it held is tried again once the hold has ended.
func TestRC8ACheckThatFailsStopsNoLaterOne(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	logs := captureLog(t)
	e.api.sendMail = func(context.Context, zmail.Message) error { panic("a mail\nthat could not be written") }
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.check()
	if !strings.Contains(logs.String(), "alerts: a check stopped at an error, and the next one runs: a mail that could not be written\n") {
		t.Fatalf("the log:\n%s", logs)
	}
	e.only(kindPart)
	e.api.sendMail = nil
	e.alerts.fail = func(method string, _ model.Alert) error {
		if method == "ActiveAlerts" {
			panic("a rule that could not be read")
		}
		return nil
	}
	e.check()
	e.alerts.fail = nil
	e.later(alertMailHold)
	e.check()
	if to, _ := e.mailed(); !slices.Equal(sorted(to), []string{"ops@example.org", "owner@example.org"}) {
		t.Fatalf("mailed to %v", to)
	}
	if !strings.Contains(logs.String(), "a rule that could not be read") {
		t.Errorf("the log:\n%s", logs)
	}
}

// R.C8: a check writes every start and every end before it sends a mail, so
// a mail server that does not answer delays no alert.
func TestRC8EveryStartAndEndIsWrittenBeforeAMail(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	since := alertNow.Add(-time.Hour)
	e.set("components", []any{part("katalog-api", false, since)})
	e.check()
	var seen []model.Alert
	send := sendThrough(e.api)
	e.api.sendMail = func(ctx context.Context, m zmail.Message) error {
		if seen == nil {
			seen = e.alerts.all()
		}
		return send(ctx, m)
	}
	e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
	e.status["certificate"] = map[string]any{"notAfter": alertNow.Add(24 * time.Hour).Format(time.RFC3339)}
	e.set("components", []any{part("katalog-api", true, time.Time{}), part("chino-api", false, since)})
	e.check()
	var got []string
	for _, a := range seen {
		state := "active"
		if a.EndedAt != nil {
			state = "ended"
		}
		got = append(got, a.Kind+" "+a.About+" "+state)
	}
	if want := []string{"part-not-ready katalog-api ended", "part-not-ready chino-api active", "certificate-ends certificate active"}; !slices.Equal(got, want) {
		t.Fatalf("at the first mail the table held %v, want %v", got, want)
	}
	if _, mails := e.mailed(); len(mails) != 8 {
		t.Fatalf("%d mails, want 8", len(mails))
	}
}

// R.C8: one portal-api holds an alert's mail at a time. Another that checks
// while the first waits for the mail server sends nothing of it, and starts
// and ends the alerts meanwhile. The holder finishes the address it was on,
// learns of the end as it writes what became of it, and makes no try of the
// start mail after the end; the end mail is due at once, and goes to the
// addresses the start mail reached, once each: an address it had not reached
// gets neither mail.
func TestRC8OnePortalAPIHoldsAnAlertsMail(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.addAdmins()
	other := e.secondAPI()
	waiting, release := make(chan struct{}), make(chan struct{})
	send := sendThrough(e.api)
	var tried []string
	e.api.sendMail = func(ctx context.Context, m zmail.Message) error {
		if tried = append(tried, m.To); len(tried) == 2 {
			close(waiting)
			<-release
		}
		return send(ctx, m)
	}
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.api.checkAlerts(context.Background())
	}()
	<-waiting
	a := e.only(kindPart)
	if h := e.alerts.heldBy(a.ID); h != e.api.holder() || h == other.holder() {
		t.Fatalf("held by %q", h)
	}
	e.later(time.Minute)
	other.checkAlerts(context.Background())
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	other.checkAlerts(context.Background())
	if a := e.only(kindPart); a.EndedAt == nil {
		t.Fatal("the other portal-api did not end it while the first waited")
	}
	if to, _ := e.mailed(); len(to) != 1 {
		t.Fatalf("mailed to %v while the first held the mail", to)
	}
	close(release)
	<-done
	if len(tried) != 2 {
		t.Fatalf("the holder tried %v: a try of the start mail after the end", tried)
	}
	// The end is due at once, and mailed by whichever portal-api runs.
	other.checkAlerts(context.Background())
	want := []string{tried[0], tried[1], tried[0], tried[1]}
	if to, _ := e.mailed(); !slices.Equal(sorted(to), sorted(want)) {
		t.Fatalf("mailed to %v at once, want the start and the end to %v", to, tried)
	}
	e.later(10 * time.Minute)
	other.checkAlerts(context.Background())
	e.api.checkAlerts(context.Background())
	to, mails := e.mailed()
	if !slices.Equal(sorted(to), sorted(want)) {
		t.Fatalf("mailed to %v: want the start and the end to %v once each", to, tried)
	}
	if subject, _, _ := readMail(t, mails[3]); subject != "Ended: A part is not ready: katalog-api" {
		t.Fatalf("the last mail %q", subject)
	}
	if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 2 {
		t.Fatalf("mail %s %d", state, n)
	}
}

// stopMidSend has e's portal-api mail an alert's start to its first address
// and then be told to stop: the tries it left are due at once.
func (e *alertEnv) stopMidSend() {
	e.t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	send, n := sendThrough(e.api), 0
	e.api.sendMail = func(ctx context.Context, m zmail.Message) error {
		n++
		if n == 1 {
			stop()
		}
		return send(ctx, m)
	}
	e.api.checkAlerts(ctx)
	e.api.sendMail = nil
	if to, _ := e.mailed(); len(to) != 1 {
		e.t.Fatalf("mailed to %v before the stop", to)
	}
}

// R.C8: an address the start mail has not reached when the alert ends gets
// neither mail: no try of the start mail is made after the end.
func TestRC8AnAddressTheStartMailHasNotReachedGetsNeither(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.addAdmins()
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.stopMidSend()
	first, _ := e.mailed()
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	e.secondAPI().checkAlerts(context.Background())
	e.later(10 * time.Minute)
	e.check()
	if to, _ := e.mailed(); !slices.Equal(to, []string{first[0], first[0]}) {
		t.Fatalf("mailed to %v: want the start and the end to %s alone", to, first[0])
	}
	if state, n, _, _ := e.mailOf(kindPart); state != mailStateSent || n != 1 {
		t.Fatalf("mail %s %d", state, n)
	}
}

// R.C8: where some addresses have the start mail and the others have not
// been tried when mail is no longer set up, or alert mail is switched off,
// the mail goes no further, and the column says it was sent to so many of so
// many, and why not to the others: “Mail is not set up.” or “Alert mail is
// off.”.
func TestRC8AMailStoppedWhereSomeHaveIt(t *testing.T) {
	for _, c := range []struct {
		name, code, message string
		stop                func(e *alertEnv)
	}{
		{"mail is no longer set up", "mail_not_set_up", "Mail is not set up.", func(e *alertEnv) { delete(e.spec, "mail") }},
		{"alert mail is switched off", "alerts_mail_off", "Alert mail is off.", func(e *alertEnv) { e.spec["alerts"] = map[string]any{"mail": false} }},
	} {
		t.Run("R.C8 "+c.name, func(t *testing.T) {
			e := newAlertEnv(t, mailtest.Options{})
			e.addAdmins()
			e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
			e.stopMidSend()
			c.stop(e)
			e.put()
			e.secondAPI().checkAlerts(context.Background())
			state, n, taken, why := e.mailOf(kindPart)
			if state != mailStateFailed || n != 3 || taken == nil || *taken != 1 || why == nil || why.Error != c.code || why.Message != c.message || len(why.Values) != 0 {
				t.Fatalf("mail %s %d %v %+v", state, n, taken, why)
			}
			if to, _ := e.mailed(); len(to) != 1 {
				t.Fatalf("mailed to %v", to)
			}
		})
	}
}

// R.C8: a start mail that never got a single try before the alert ended gets
// neither mail, and the column says that it ended before its mail went.
func TestRC8AnAlertThatEndedBeforeItsMailWent(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	// The check that starts it has too little time left for a mail.
	e.api.alertLoop.checkTime = zmail.Timeout - time.Second
	e.check()
	e.api.alertLoop.checkTime = 0
	e.later(time.Minute)
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	e.check()
	e.later(10 * time.Minute)
	e.check()
	if to, _ := e.mailed(); len(to) != 0 {
		t.Fatalf("mailed to %v", to)
	}
	if state, n, taken, why := e.mailOf(kindPart); state != "ended-before-mail" || n != 0 || taken != nil || why != nil {
		t.Fatalf("mail %s %d %v %+v", state, n, taken, why)
	}
}

// R.C8: a portal-api told to stop while it sends finishes the address it is
// on and writes what became of it; another makes the tries it left at once,
// and each address gets the mail once.
func TestRC8APortalAPIThatStopsFinishesTheAddressItIsOn(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.addAdmins()
	other := e.secondAPI()
	ctx, stop := context.WithCancel(context.Background())
	send, n := sendThrough(e.api), 0
	e.api.sendMail = func(ctx context.Context, m zmail.Message) error {
		n++
		if n == 1 {
			stop()
		}
		return send(ctx, m)
	}
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.api.checkAlerts(ctx)
	if to, _ := e.mailed(); len(to) != 1 {
		t.Fatalf("the one that stopped mailed %v", to)
	}
	r := e.startMail(kindPart)
	if sent := len(r.taken()); sent != 1 || len(r.To) != 3 {
		t.Fatalf("what became of it: %+v", r)
	}
	if h := e.alerts.heldBy(e.only(kindPart).ID); h != "" {
		t.Fatalf("still held by %q", h)
	}
	other.checkAlerts(context.Background())
	if to, _ := e.mailed(); !slices.Equal(sorted(to), []string{"bert@example.org", "ops@example.org", "owner@example.org"}) {
		t.Fatalf("mailed to %v", to)
	}
}

// R.C8: a check has a time of its own, and so has each call of the portal's
// database in it: with too little time left for a mail, the check sends none
// and lets the tries go, at once, to whichever portal-api runs next.
func TestRC8ACheckHasATimeOfItsOwn(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	var longest time.Duration
	withoutOne := 0
	e.api.alerts = deadlines{e.alerts, func(ctx context.Context) {
		if d, ok := ctx.Deadline(); ok {
			longest = max(longest, time.Until(d))
		} else {
			withoutOne++
		}
	}}
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.api.alertLoop.checkTime = zmail.Timeout - time.Second
	e.api.checkAlerts(context.Background())
	if n := len(e.smtp.Sessions()); n != 0 {
		t.Fatalf("%d sessions with too little time left", n)
	}
	a := e.only(kindPart)
	if e.alerts.heldBy(a.ID) != "" || a.MailNext == nil || a.MailNext.After(e.clock) {
		t.Fatalf("its tries were not let go: %+v", a)
	}
	// So does a check whose caller has too little time left.
	e.api.alertLoop.checkTime = 0
	ctx, cancel := context.WithTimeout(context.Background(), zmail.Timeout-time.Second)
	defer cancel()
	e.api.checkAlerts(ctx)
	if n := len(e.smtp.Sessions()); n != 0 {
		t.Fatalf("%d sessions with too little time left", n)
	}
	e.api.checkAlerts(context.Background())
	if to, _ := e.mailed(); len(to) != 2 {
		t.Fatalf("mailed to %v", to)
	}
	if withoutOne != 0 || longest > alertCallTime {
		t.Fatalf("%d calls without a deadline, the longest %s", withoutOne, longest)
	}
}

// R.C8: what became of a mail is kept after each address, so a portal-api
// that stops at once while it sends repeats the mail to one address at most.
func TestRC8WhatBecameOfAMailIsKeptAfterEachAddress(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.addAdmins()
	other := e.secondAPI()
	send, n := sendThrough(e.api), 0
	e.api.sendMail = func(ctx context.Context, m zmail.Message) error {
		n++
		err := send(ctx, m)
		if n == 2 {
			panic("portal-api stops at once")
		}
		return err
	}
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
	e.api.checkAlerts(context.Background())
	first, _ := e.mailed()
	e.later(alertMailHold)
	other.checkAlerts(context.Background())
	to, _ := e.mailed()
	if len(first) != 2 || !slices.Equal(sorted(to), sorted(append(slices.Clone(first), first[1], third(first)))) {
		t.Fatalf("first %v, then %v: the second address once more, the third once", first, to)
	}
}

// third is the alert's address that is not among first.
func third(first []string) string {
	for _, a := range []string{"bert@example.org", "ops@example.org", "owner@example.org"} {
		if !slices.Contains(first, a) {
			return a
		}
	}
	return ""
}

// deadlines is an alert store that tells seen each call's context.
type deadlines struct {
	*fakeAlerts
	seen func(ctx context.Context)
}

func (d deadlines) StartAlert(ctx context.Context, in store.NewAlert) (model.Alert, bool, error) {
	d.seen(ctx)
	return d.fakeAlerts.StartAlert(ctx, in)
}

func (d deadlines) ActiveAlerts(ctx context.Context) ([]model.Alert, error) {
	d.seen(ctx)
	return d.fakeAlerts.ActiveAlerts(ctx)
}

func (d deadlines) RewriteAlert(ctx context.Context, id, title, text string, values, memo json.RawMessage) error {
	d.seen(ctx)
	return d.fakeAlerts.RewriteAlert(ctx, id, title, text, values, memo)
}

func (d deadlines) EndAlert(ctx context.Context, id string, at time.Time, text string, values json.RawMessage, holder string, hold time.Duration) (model.Alert, bool, bool, error) {
	d.seen(ctx)
	return d.fakeAlerts.EndAlert(ctx, id, at, text, values, holder, hold)
}

func (d deadlines) StartMailedWithin(ctx context.Context, kind, about string, within time.Duration) (bool, error) {
	d.seen(ctx)
	return d.fakeAlerts.StartMailedWithin(ctx, kind, about, within)
}

func (d deadlines) DueMail(ctx context.Context, limit int) ([]string, error) {
	d.seen(ctx)
	return d.fakeAlerts.DueMail(ctx, limit)
}

func (d deadlines) LeaseMail(ctx context.Context, id, holder string, hold time.Duration) (model.Alert, bool, error) {
	d.seen(ctx)
	return d.fakeAlerts.LeaseMail(ctx, id, holder, hold)
}

func (d deadlines) KeepMail(ctx context.Context, id, holder string, mail json.RawMessage, startMailed bool, hold time.Duration) (bool, bool, error) {
	d.seen(ctx)
	return d.fakeAlerts.KeepMail(ctx, id, holder, mail, startMailed, hold)
}

func (d deadlines) ReleaseMail(ctx context.Context, id, holder string, mail json.RawMessage, next *time.Duration, sawEnded bool) (bool, error) {
	d.seen(ctx)
	return d.fakeAlerts.ReleaseMail(ctx, id, holder, mail, next, sawEnded)
}

func (d deadlines) SweepAlerts(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	d.seen(ctx)
	return d.fakeAlerts.SweepAlerts(ctx, cutoff, batch)
}
