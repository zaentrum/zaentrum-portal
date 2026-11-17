package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/db"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// alertEpoch is when the alerts of these tests start.
var alertEpoch = time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)

// hold is how long a portal-api of these tests holds an alert's mail.
const hold = 2 * time.Minute

// alertStore is a migrated store: every boot re-applies every migration.
func alertStore(t *testing.T) *Store {
	t.Helper()
	st := testStore(t)
	for i := 0; i < 2; i++ {
		if err := st.Migrate(context.Background(), db.Migrations, "zaentrum-admin"); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func newAlert(kind, about string, at time.Time) NewAlert {
	return NewAlert{
		Kind: kind, About: about, Title: "A part is not ready: " + about, Text: about + " has not been ready for more than 10 minutes.",
		Link: "/portal/operator", Values: json.RawMessage(`{"part":"` + about + `","minutes":10}`), At: at, Holder: "first", Hold: hold,
	}
}

func start(t *testing.T, st *Store, kind, about string, at time.Time) model.Alert {
	t.Helper()
	a, won, err := st.StartAlert(context.Background(), newAlert(kind, about, at))
	if err != nil || !won {
		t.Fatalf("start %s about %s: won %v, %v", kind, about, won, err)
	}
	return a
}

// set runs a statement on one alert: the database's clock cannot be moved,
// so its lease and its next try are moved instead.
func set(t *testing.T, st *Store, id, assignments string) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), `UPDATE alerts SET `+assignments+` WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
}

// R.C8: one alert of a kind is active for one subject; it starts again once
// the one before has ended. Its title, text, values and link are kept as they
// came; its mail is due at once, held by the portal-api that started it.
func TestRC8AnAlertIsActiveOnceForItsKindAndSubject(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	first := start(t, st, "part-not-ready", "katalog-api", alertEpoch)
	if first.Title != "A part is not ready: katalog-api" || first.Link != "/portal/operator" || first.EndedAt != nil ||
		!first.StartedAt.Equal(alertEpoch) || first.MailNext == nil {
		t.Fatalf("started as %+v", first)
	}
	var values map[string]any
	if err := json.Unmarshal(first.Values, &values); err != nil || values["part"] != "katalog-api" || values["minutes"] != float64(10) {
		t.Fatalf("values %s: %v", first.Values, err)
	}
	if _, won, err := st.StartAlert(ctx, newAlert("part-not-ready", "katalog-api", alertEpoch.Add(time.Minute))); err != nil || won {
		t.Fatalf("a second start of the same alert won: %v %v", won, err)
	}
	start(t, st, "part-not-ready", "chino-api", alertEpoch)  // another subject
	start(t, st, "update-failed", "katalog-api", alertEpoch) // another kind, the same name
	if _, won, _, err := st.EndAlert(ctx, first.ID, alertEpoch.Add(time.Hour), "katalog-api is ready again.", nil, "first", hold); err != nil || !won {
		t.Fatalf("end: won %v, %v", won, err)
	}
	again := start(t, st, "part-not-ready", "katalog-api", alertEpoch.Add(2*time.Hour))
	if again.ID == first.ID {
		t.Error("the alert that started again is the one that ended")
	}
	active, err := st.ActiveAlerts(ctx)
	if err != nil || len(active) != 3 {
		t.Fatalf("active: %d, %v", len(active), err)
	}
}

// R.C8: of portal-apis that start the same alert at once, one insert wins —
// and only that one holds its mail.
func TestRC8TwoStartsAtOnceStartOneAlert(t *testing.T) {
	st := alertStore(t)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, won, err := st.StartAlert(context.Background(), newAlert("volume-full", "media", alertEpoch))
			if err != nil {
				t.Error(err)
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d starts won, want 1", wins)
	}
}

// R.C8: an alert ends once. The write that ends it answers it, and holds its
// mail where nobody else holds it, by the database's clock; an alert that
// ended keeps its last title and text, which a later write does not change.
func TestRC8AnAlertEndsOnce(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	a := start(t, st, "part-not-ready", "katalog-api", alertEpoch)
	if err := st.RewriteAlert(ctx, a.ID, "A part is not ready: katalog-api", "the text now", json.RawMessage(`{"part":"katalog-api","minutes":15}`), nil); err != nil {
		t.Fatal(err)
	}
	// The start's portal-api still holds the mail: another's end leaves it.
	ended, won, held, err := st.EndAlert(ctx, a.ID, alertEpoch.Add(5*time.Minute), "katalog-api is ready again.", json.RawMessage(`{"part":"katalog-api"}`), "second", hold)
	if err != nil || !won || held {
		t.Fatalf("end: won %v held %v, %v", won, held, err)
	}
	if ended.EndedAt == nil || !ended.EndedAt.Equal(alertEpoch.Add(5*time.Minute)) || ended.EndText != "katalog-api is ready again." || ended.Text != "the text now" {
		t.Fatalf("ended as %+v", ended)
	}
	if _, won, _, err := st.EndAlert(ctx, a.ID, alertEpoch.Add(6*time.Minute), "again", nil, "second", hold); err != nil || won {
		t.Fatalf("a second end won: %v %v", won, err)
	}
	if err := st.RewriteAlert(ctx, a.ID, "changed", "changed", nil, nil); err != nil {
		t.Fatal(err)
	}
	list, _, _, err := st.Alerts(ctx, "admin", alertEpoch.Add(-time.Hour), alertEpoch.Add(-time.Hour), 500)
	if err != nil || len(list) != 1 || list[0].Title != "A part is not ready: katalog-api" || list[0].Text != "the text now" {
		t.Fatalf("after the end: %+v, %v", list, err)
	}

	// The start's lease has run out: the end's write holds the mail; and the
	// portal-api that holds it already holds it on.
	b := start(t, st, "part-not-ready", "chino-api", alertEpoch)
	set(t, st, b.ID, "mail_lease = now() - interval '1 second'")
	if _, won, held, err := st.EndAlert(ctx, b.ID, alertEpoch.Add(16*time.Minute), "chino-api is ready again.", nil, "second", hold); err != nil || !won || !held {
		t.Fatalf("end after the lease: won %v held %v, %v", won, held, err)
	}
	c := start(t, st, "part-not-ready", "katalog-ingest", alertEpoch)
	if _, _, held, err := st.EndAlert(ctx, c.ID, alertEpoch.Add(time.Minute), "katalog-ingest is ready again.", nil, "first", hold); err != nil || !held {
		t.Fatalf("its own holder: held %v, %v", held, err)
	}
	// Ends at once: one wins.
	d := start(t, st, "part-not-ready", "packager", alertEpoch)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, won, _, err := st.EndAlert(context.Background(), d.ID, alertEpoch.Add(time.Hour), "packager is ready again.", nil, "second", hold)
			if err != nil {
				t.Error(err)
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d ends won, want 1", wins)
	}
	if _, _, _, err := st.EndAlert(ctx, "not-an-id", alertEpoch, "x", nil, "second", hold); !errors.Is(err, ErrNotFound) {
		t.Errorf("an id that is none: %v", err)
	}
}

// R.C8: an alert's mail is held by one portal-api at a time, until its lease
// ends by the database's clock: only its holder writes what became of it,
// each write keeps the lease, and a portal-api whose lease another has taken
// writes nothing; a release has the next try due when it says, and the end
// mail at once where another portal-api ended the alert meanwhile.
func TestRC8AnAlertsMailIsHeldByOne(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	a := start(t, st, "certificate-ends", "certificate", alertEpoch)
	if due, err := st.DueMail(ctx, 50); err != nil || len(due) != 0 {
		t.Fatalf("held by its start, and due for another: %v, %v", due, err)
	}
	if _, ok, err := st.LeaseMail(ctx, a.ID, "second", hold); err != nil || ok {
		t.Fatalf("leased by another while held: %v %v", ok, err)
	}
	if ok, _, err := st.KeepMail(ctx, a.ID, "second", json.RawMessage(`{"by":"second"}`), true, hold); err != nil || ok {
		t.Fatalf("written by another: %v %v", ok, err)
	}
	if _, ok, err := st.LeaseMail(ctx, a.ID, "first", hold); err != nil || !ok {
		t.Fatalf("its holder, right before the mail goes: %v %v", ok, err)
	}
	if ok, _, err := st.KeepMail(ctx, a.ID, "first", json.RawMessage(`{"start":{"begun":true,"to":[{"address":"anna@example.org","sent":true}]}}`), true, hold); err != nil || !ok {
		t.Fatalf("kept by its holder: %v %v", ok, err)
	}
	next := 5 * time.Minute
	if ok, err := st.ReleaseMail(ctx, a.ID, "first", json.RawMessage(`{"start":{"begun":true}}`), &next, false); err != nil || !ok {
		t.Fatalf("released: %v %v", ok, err)
	}
	if due, err := st.DueMail(ctx, 50); err != nil || len(due) != 0 {
		t.Fatalf("due before its next try: %v, %v", due, err)
	}
	set(t, st, a.ID, "mail_next = now() - interval '1 second'")
	// Due: of portal-apis that take it at once, one does.
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		takes int
	)
	for _, holder := range []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := st.LeaseMail(context.Background(), a.ID, holder, hold); err != nil {
				t.Error(err)
			} else if ok {
				mu.Lock()
				takes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if takes != 1 {
		t.Fatalf("taken %d times, want 1", takes)
	}
	// Its lease runs out: another takes it, and the one before writes
	// nothing.
	var holder string
	if err := st.pool.QueryRow(ctx, `SELECT mail_holder FROM alerts WHERE id = $1::uuid`, a.ID).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	set(t, st, a.ID, "mail_lease = now() - interval '1 second'")
	if due, err := st.DueMail(ctx, 50); err != nil || len(due) != 1 || due[0] != a.ID {
		t.Fatalf("due once the lease ran out: %v, %v", due, err)
	}
	taken, ok, err := st.LeaseMail(ctx, a.ID, "next", hold)
	if err != nil || !ok || string(taken.Mail) != `{"start": {"begun": true}}` || taken.StartMailedAt == nil {
		t.Fatalf("after the lease: %+v %v, %v", taken, ok, err)
	}
	if ok, _, err := st.KeepMail(ctx, a.ID, holder, json.RawMessage(`{"overwritten":true}`), false, hold); err != nil || ok {
		t.Fatalf("written by the one whose lease ran out: %v %v", ok, err)
	}
	if ok, err := st.ReleaseMail(ctx, a.ID, holder, json.RawMessage(`{"overwritten":true}`), nil, false); err != nil || ok {
		t.Fatalf("released by the one whose lease ran out: %v %v", ok, err)
	}
	if ok, err := st.ReleaseMail(ctx, a.ID, "next", json.RawMessage(`{"start":{"begun":true,"held":"off"}}`), nil, false); err != nil || !ok {
		t.Fatalf("released by its holder: %v %v", ok, err)
	}
	if due, err := st.DueMail(ctx, 50); err != nil || len(due) != 0 {
		t.Fatalf("nothing due, and listed: %v, %v", due, err)
	}

	// One portal-api carries a start mail on while another ends the alert:
	// the first one learns of the end with its next write, and its release
	// leaves the end mail due.
	b := start(t, st, "part-not-ready", "katalog-api", alertEpoch.Add(time.Hour))
	if ok, ended, err := st.KeepMail(ctx, b.ID, "first", json.RawMessage(`{"start":{"begun":true}}`), false, hold); err != nil || !ok || ended {
		t.Fatalf("before the end: held %v ended %v, %v", ok, ended, err)
	}
	if _, won, held, err := st.EndAlert(ctx, b.ID, alertEpoch.Add(time.Hour+time.Minute), "katalog-api is ready again.", nil, "second", hold); err != nil || !won || held {
		t.Fatalf("end while held: won %v held %v, %v", won, held, err)
	}
	if ok, ended, err := st.KeepMail(ctx, b.ID, "first", json.RawMessage(`{"start":{"begun":true}}`), false, hold); err != nil || !ok || !ended {
		t.Fatalf("after the end: held %v ended %v, %v", ok, ended, err)
	}
	if ok, err := st.ReleaseMail(ctx, b.ID, "first", json.RawMessage(`{"start":{"begun":true}}`), nil, false); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if due, err := st.DueMail(ctx, 50); err != nil || len(due) != 1 || due[0] != b.ID {
		t.Fatalf("the end mail is not due: %v, %v", due, err)
	}
	if _, ok, err := st.LeaseMail(ctx, b.ID, "second", hold); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := st.ReleaseMail(ctx, b.ID, "second", json.RawMessage(`{"start":{"begun":true}}`), nil, true); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if due, err := st.DueMail(ctx, 50); err != nil || len(due) != 0 {
		t.Fatalf("due after the end was carried: %v, %v", due, err)
	}

	// Each write keeps the hold, for the hold from the write: a lease about
	// to run out holds on after its holder wrote.
	c := start(t, st, "volume-full", "media", alertEpoch.Add(2*time.Hour))
	set(t, st, c.ID, "mail_lease = now() + interval '300 milliseconds'")
	if ok, _, err := st.KeepMail(ctx, c.ID, "first", json.RawMessage(`{"start":{"begun":true}}`), false, hold); err != nil || !ok {
		t.Fatal(ok, err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, ok, err := st.LeaseMail(ctx, c.ID, "second", hold); err != nil || ok {
		t.Fatalf("taken by another after its holder wrote: %v %v", ok, err)
	}
	if ok, _, err := st.KeepMail(ctx, c.ID, "first", json.RawMessage(`{"start":{"begun":true}}`), false, hold); err != nil || !ok {
		t.Fatalf("its holder lost it: %v %v", ok, err)
	}
}

// R.C8: the 24 hours in which one start mail is sent count from when the mail
// server first took the mail of the last start of that kind about that
// subject, by the database's clock.
func TestRC8OneStartMailIn24Hours(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	a := start(t, st, "volume-full", "media", alertEpoch)
	if yes, err := st.StartMailedWithin(ctx, "volume-full", "media", 24*time.Hour); err != nil || yes {
		t.Fatalf("mailed before it was: %v %v", yes, err)
	}
	if ok, _, err := st.KeepMail(ctx, a.ID, "first", json.RawMessage(`{}`), true, hold); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var first time.Time
	if err := st.pool.QueryRow(ctx, `SELECT start_mailed_at FROM alerts WHERE id = $1::uuid`, a.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	// A later address keeps the first time.
	if ok, _, err := st.KeepMail(ctx, a.ID, "first", json.RawMessage(`{}`), true, hold); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var again time.Time
	if err := st.pool.QueryRow(ctx, `SELECT start_mailed_at FROM alerts WHERE id = $1::uuid`, a.ID).Scan(&again); err != nil || !again.Equal(first) {
		t.Fatalf("the first time moved: %v %v, %v", first, again, err)
	}
	for _, c := range []struct {
		kind, about, mailed string
		want                bool
	}{
		{"volume-full", "media", "now() - interval '23 hours'", true},
		{"volume-full", "media", "now() - interval '24 hours 1 second'", false},
		{"volume-full", "backups", "now() - interval '1 hour'", false},
		{"part-not-ready", "media", "now() - interval '1 hour'", false},
	} {
		set(t, st, a.ID, "start_mailed_at = "+c.mailed)
		if yes, err := st.StartMailedWithin(ctx, c.kind, c.about, 24*time.Hour); err != nil || yes != c.want {
			t.Errorf("%s about %s mailed %s: %v %v, want %v", c.kind, c.about, c.mailed, yes, err, c.want)
		}
	}
}

// R.C8: an admin has seen an alert when they have seen it since it last
// changed — when it started, or when it ended; unseen counts what the bell
// shows, the active alerts and those that ended within 7 days; the list is
// the active ones first, the newest first, then those that ended, the newest
// end first; read-all marks only what was unseen.
func TestRC8AnAlertIsSeenSinceItLastChanged(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	old := start(t, st, "certificate-ends", "certificate", alertEpoch.Add(-20*24*time.Hour))
	if _, _, _, err := st.EndAlert(ctx, old.ID, alertEpoch.Add(-10*24*time.Hour), "The certificate now ends on 1 March 2027, 00:00 UTC.", nil, "first", hold); err != nil {
		t.Fatal(err)
	}
	a := start(t, st, "part-not-ready", "katalog-api", alertEpoch)
	b := start(t, st, "no-good-backup", "backups", alertEpoch.Add(time.Hour))
	now := alertEpoch.Add(2 * time.Hour)
	bell, since := now.Add(-7*24*time.Hour), now.Add(-90*24*time.Hour)
	list, _, unseen, err := st.Alerts(ctx, "anna", since, bell, 500)
	if err != nil || unseen != 2 || len(list) != 3 {
		t.Fatalf("unseen %d, %d listed, %v", unseen, len(list), err)
	}
	if got := []string{list[0].ID, list[1].ID, list[2].ID}; got[0] != b.ID || got[1] != a.ID || got[2] != old.ID {
		t.Fatalf("order %v: want the active ones newest first, then the ended", got)
	}
	if n, err := st.SeeAlert(ctx, "anna", a.ID, now, bell); err != nil || n != 1 {
		t.Fatalf("after one: unseen %d, %v", n, err)
	}
	list, _, _, _ = st.Alerts(ctx, "anna", since, bell, 500)
	if !list[1].Seen || list[0].Seen {
		t.Fatalf("seen: %+v", list)
	}
	if _, _, unseen, _ := st.Alerts(ctx, "bert", since, bell, 500); unseen != 2 {
		t.Fatalf("another admin's: %d", unseen)
	}
	// It ends: unseen again.
	if _, _, _, err := st.EndAlert(ctx, a.ID, now.Add(time.Minute), "katalog-api is ready again.", nil, "first", hold); err != nil {
		t.Fatal(err)
	}
	if _, _, unseen, _ := st.Alerts(ctx, "anna", since, bell, 500); unseen != 2 {
		t.Fatalf("after the end: unseen %d, want 2", unseen)
	}
	// Read-all marks the three unseen, and leaves a mark that holds.
	if _, err := st.SeeAlert(ctx, "anna", b.ID, now.Add(90*time.Second), bell); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SeeAllAlerts(ctx, "anna", now.Add(2*time.Minute)); err != nil || n != 2 {
		t.Fatalf("all: %d, %v, want the 2 unseen", n, err)
	}
	var kept time.Time
	if err := st.pool.QueryRow(ctx, `SELECT seen_at FROM alert_seen WHERE alert_id = $1::uuid AND sub = 'anna'`, b.ID).Scan(&kept); err != nil || !kept.Equal(now.Add(90*time.Second)) {
		t.Fatalf("a seen mark that held was written again: %v, %v", kept, err)
	}
	if list, _, unseen, _ := st.Alerts(ctx, "anna", since, bell, 500); unseen != 0 || !list[0].Seen || !list[1].Seen || !list[2].Seen {
		t.Fatalf("after all: unseen %d %+v", unseen, list)
	}
	for _, id := range []string{"", "1", "not-a-uuid", strings.ToUpper(a.ID), "00000000-0000-0000-0000-000000000000", a.ID + "'; DROP TABLE alerts; --"} {
		if _, err := st.SeeAlert(ctx, "anna", id, now, bell); !errors.Is(err, ErrNotFound) {
			t.Errorf("SeeAlert(%q) = %v, want ErrNotFound", id, err)
		}
	}
	// An alert that ended before since is not listed.
	if list, _, _, _ := st.Alerts(ctx, "anna", now.Add(-5*24*time.Hour), bell, 500); len(list) != 2 {
		t.Fatalf("since 5 days: %d listed", len(list))
	}
}

// R.C8: the list holds every active alert and the newest limit that ended,
// the newest end first, and says where older ones are kept.
func TestRC8TheListOfAlertsIsBounded(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	for i, about := range []string{"a", "b", "c"} {
		x := start(t, st, "part-not-ready", about, alertEpoch.Add(time.Duration(i)*time.Hour))
		if _, _, _, err := st.EndAlert(ctx, x.ID, alertEpoch.Add(time.Duration(10-i)*time.Hour), about+" is ready again.", nil, "first", hold); err != nil {
			t.Fatal(err)
		}
	}
	start(t, st, "part-not-ready", "d", alertEpoch)
	start(t, st, "part-not-ready", "e", alertEpoch.Add(time.Hour))
	list, more, _, err := st.Alerts(ctx, "anna", alertEpoch.Add(-24*time.Hour), alertEpoch, 2)
	if err != nil || !more || len(list) != 4 {
		t.Fatalf("%d listed, more %v, %v", len(list), more, err)
	}
	if got := []string{list[0].About, list[1].About, list[2].About, list[3].About}; strings.Join(got, ",") != "e,d,a,b" {
		t.Fatalf("listed %v, want the active newest first, then the newest ends", got)
	}
	if _, more, _, _ := st.Alerts(ctx, "anna", alertEpoch.Add(-24*time.Hour), alertEpoch, 3); more {
		t.Fatal("more where none is older")
	}
}

// R.C8: an alert is kept until 90 days after it ended, and an active one
// however old; the sweep goes a batch at a time, and an alert's seen marks go
// with it.
func TestRC8AlertsAreKeptUntil90DaysAfterTheyEnded(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	var gone []string
	for _, about := range []string{"a", "b", "c", "d", "e"} {
		x := start(t, st, "volume-full", about, alertEpoch.Add(-200*24*time.Hour))
		if _, _, _, err := st.EndAlert(ctx, x.ID, alertEpoch.Add(-91*24*time.Hour), "The library volume is at 70 %.", nil, "first", hold); err != nil {
			t.Fatal(err)
		}
		gone = append(gone, x.ID)
	}
	kept := start(t, st, "volume-full", "backups", alertEpoch.Add(-100*24*time.Hour))
	if _, _, _, err := st.EndAlert(ctx, kept.ID, alertEpoch.Add(-89*24*time.Hour), "The backups volume is at 70 %.", nil, "first", hold); err != nil {
		t.Fatal(err)
	}
	active := start(t, st, "update-failed", "update", alertEpoch.Add(-300*24*time.Hour))
	if _, err := st.SeeAlert(ctx, "anna", gone[0], alertEpoch, alertEpoch); err != nil {
		t.Fatal(err)
	}
	n, err := st.SweepAlerts(ctx, alertEpoch.Add(-90*24*time.Hour), 2)
	if err != nil || n != 5 {
		t.Fatalf("swept %d, %v, want 5 in batches of 2", n, err)
	}
	list, _, _, _ := st.Alerts(ctx, "anna", alertEpoch.Add(-1000*24*time.Hour), alertEpoch, 500)
	if len(list) != 2 || list[0].ID != active.ID || list[1].ID != kept.ID {
		t.Fatalf("left: %+v", list)
	}
	var marks int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM alert_seen`).Scan(&marks); err != nil || marks != 0 {
		t.Fatalf("seen marks left: %d, %v", marks, err)
	}
}

// R.C8: the table holds an alert to its words' limits — a title of at most 80
// characters and a text of at most 280 — to the five kinds, and to a subject
// of at most 255 characters.
func TestRC8AnAlertKeepsItsLimits(t *testing.T) {
	st := alertStore(t)
	ctx := context.Background()
	at := func(in NewAlert) NewAlert { in.At, in.Holder, in.Hold = alertEpoch, "first", hold; return in }
	for name, in := range map[string]NewAlert{
		"a title of 81":    at(NewAlert{Kind: "part-not-ready", About: "a", Title: strings.Repeat("é", 81), Text: "t"}),
		"a text of 281":    at(NewAlert{Kind: "part-not-ready", About: "b", Title: "t", Text: strings.Repeat("é", 281)}),
		"another kind":     at(NewAlert{Kind: "disk-on-fire", About: "c", Title: "t", Text: "t"}),
		"no subject":       at(NewAlert{Kind: "part-not-ready", About: "", Title: "t", Text: "t"}),
		"a subject of 256": at(NewAlert{Kind: "part-not-ready", About: strings.Repeat("p", 256), Title: "t", Text: "t"}),
	} {
		if _, _, err := st.StartAlert(ctx, in); err == nil {
			t.Errorf("%s was kept", name)
		}
	}
	for name, in := range map[string]NewAlert{
		"a title of 80":    at(NewAlert{Kind: "part-not-ready", About: "d", Title: strings.Repeat("é", 80), Text: "t"}),
		"a text of 280":    at(NewAlert{Kind: "part-not-ready", About: "e", Title: "t", Text: strings.Repeat("é", 280)}),
		"a subject of 255": at(NewAlert{Kind: "part-not-ready", About: strings.Repeat("p", 255), Title: "t", Text: "t"}),
	} {
		if _, won, err := st.StartAlert(ctx, in); err != nil || !won {
			t.Errorf("%s: %v %v", name, won, err)
		}
	}
}
