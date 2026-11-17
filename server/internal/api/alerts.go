package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// ─── alerts ──────────────────────────────────────────────────────────────────
//
// An alert says that one of five things is wrong with the platform (section
// Alerts of the chapter Running the platform). portal-api reads the status of
// the platform's resource every 60 seconds, starts and ends the alerts by the
// rules (alertrules.go), keeps them in a table of its own until 90 days after
// they ended, and mails them (alertmail.go). The routes, all of them for
// admins:
//
//	GET   /alerts             every active alert and the newest 500 that ended in the last 90 days, how many the bell shows that the admin has not seen, and what is watched
//	PATCH /alerts/settings    any of the four thresholds and mail, into spec.alerts
//	POST  /alerts/{id}/read   one alert, seen by the admin who asks
//	POST  /alerts/read-all    every alert the admin had not seen, seen by them
//
// A platform whose resource is younger than 1 hour raises no alert, and one
// the operator does not run raises none. A check writes every start and every
// end before it sends a mail, so a mail server that does not answer delays no
// alert; a check has a time of its own, and so has each call of the portal's
// database in it; and a check that fails stops no later one.

const (
	// alertCheckEvery is how often the status is read; alertCheckTime how
	// long one check may take in all, and alertCallTime one call of the
	// portal's database in it.
	alertCheckEvery = 60 * time.Second
	alertCheckTime  = 50 * time.Second
	alertCallTime   = 10 * time.Second
	// alertSweepEvery is how often the alerts past their keeping go, and
	// alertSweepBatch how many go with one statement.
	alertSweepEvery = time.Hour
	alertSweepBatch = 1000
	// alertKept is how long an alert is kept after it ended; alertInBell how
	// long one that ended stays in the bell.
	alertKept   = 90 * 24 * time.Hour
	alertInBell = 7 * 24 * time.Hour
	// alertQuiet is how old the platform's resource is before it raises an
	// alert.
	alertQuiet = time.Hour
	// alertsKnownFor is how long the cluster's answer is kept to whether it
	// knows alert settings.
	alertsKnownFor = 5 * time.Minute
	// alertMailBatch is the most alerts whose tries one check takes.
	alertMailBatch = 50
	// alertsListed is the most alerts that ended GET /alerts answers.
	alertsListed = 500
	// maxAlertsBody bounds what PATCH /alerts/settings reads.
	maxAlertsBody = 8 << 10
)

// alertStore is the alerts as the loop and the routes keep them.
// *store.Store implements it; tests substitute an in-memory one.
type alertStore interface {
	StartAlert(ctx context.Context, in store.NewAlert) (model.Alert, bool, error)
	ActiveAlerts(ctx context.Context) ([]model.Alert, error)
	RewriteAlert(ctx context.Context, id, title, text string, values, memo json.RawMessage) error
	EndAlert(ctx context.Context, id string, at time.Time, text string, values json.RawMessage, holder string, hold time.Duration) (model.Alert, bool, bool, error)
	StartMailedWithin(ctx context.Context, kind, about string, within time.Duration) (bool, error)
	DueMail(ctx context.Context, limit int) ([]string, error)
	LeaseMail(ctx context.Context, id, holder string, hold time.Duration) (model.Alert, bool, error)
	KeepMail(ctx context.Context, id, holder string, mail json.RawMessage, startMailed bool, hold time.Duration) (bool, bool, error)
	ReleaseMail(ctx context.Context, id, holder string, mail json.RawMessage, next *time.Duration, sawEnded bool) (bool, error)
	Alerts(ctx context.Context, sub string, since, bellSince time.Time, limit int) ([]model.Alert, bool, int, error)
	SeeAlert(ctx context.Context, sub, id string, at, bellSince time.Time) (int, error)
	SeeAllAlerts(ctx context.Context, sub string, at time.Time) (int, error)
	SweepAlerts(ctx context.Context, cutoff time.Time, batch int) (int, error)
}

// alertLoop is what the checks keep between them: what they last said in the
// log, when the alerts were last swept, the cluster's last answer to whether
// it knows alert settings, and how this portal-api names itself as the holder
// of an alert's mail.
type alertLoop struct {
	mu         sync.Mutex
	unreadable bool
	young      bool
	noOperator bool
	swept      time.Time
	known      struct {
		at       time.Time
		resource string
		yes      bool
	}
	holderOnce sync.Once
	holder     string
	// ticker makes the loop's ticks, a ticker of the time package, and
	// checkTime is how long a check may take, alertCheckTime: unless a test
	// sets them.
	ticker    func(every time.Duration) (<-chan time.Time, func())
	checkTime time.Duration
}

// The refusals of the alerts' routes. A sentence the alerts page words its
// own way has a code that starts with alerts_; reading the platform's
// resource, and a cluster that answers otherwise, are worded as on every page
// (mail.go).
var (
	refuseAlertsNotRunByOperator = refusal{Error: "alerts_not_run_by_operator", Message: "This platform is not run by the operator, so nothing watches it from here."}
	refuseAlertsMayNotChange     = refusal{Error: "alerts_write_forbidden", Message: "The portal is not allowed to change the alert settings. Update the operator, which gives the portal that right."}
	refuseAlertsRefused          = refusal{Error: "alerts_write_refused", Message: "The cluster refused the alert settings."}
	refuseAlertsUnknown          = refusal{Error: "alerts_settings_unknown", Message: "The cluster does not know alert settings. Update the operator, which brings them."}
)

// refuseThreshold is a threshold outside its range, or of the wrong kind: the
// field, and the least and the most it takes.
func refuseThreshold(s operator.AlertSetting) refusal {
	return refusal{
		Error:   "alerts_threshold_out_of_range",
		Values:  map[string]any{"field": s.Field, "least": s.Least, "most": s.Most},
		Message: "A number from " + strconv.Itoa(s.Least) + " to " + strconv.Itoa(s.Most) + ".",
	}
}

// spaced is s with each control character, a line break among them, written
// as a space, as every line of the alerts' log has it.
func spaced(s string) string { return operator.Spaced(s) }

// holder is this portal-api as the alerts table names the one that holds an
// alert's mail: made once, at random.
func (a *API) holder() string {
	a.alertLoop.holderOnce.Do(func() {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		a.alertLoop.holder = "portal-api-" + hex.EncodeToString(b)
	})
	return a.alertLoop.holder
}

// callContext is one call of the portal's database within a check: within
// the check's time, and no longer than alertCallTime.
func callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, alertCallTime)
}

// dbContext is a write of what became of a mail: made whatever stops
// portal-api or ends the check meanwhile, so that the address the mail was on
// is kept, and no longer than alertCallTime.
func dbContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), alertCallTime)
}

// alertStopping: portal-api stops, or the check has too little time left for
// one more mail. The tries left are made by whichever portal-api runs next.
func alertStopping(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < mail.Timeout
}

// ─── the loop ────────────────────────────────────────────────────────────────

// RunAlerts checks the alerts at once and every 60 seconds until ctx ends. A
// check that runs when ctx ends finishes the address it is on, writes what
// became of it, and stops.
func (a *API) RunAlerts(ctx context.Context) {
	if a.alerts == nil {
		return
	}
	if a.op == nil || !a.op.Available() {
		log.Printf("alerts: the platform is not run by the operator, so no alert is raised")
		return
	}
	log.Printf("alerts: the platform's status is read every 60s; an alert is kept 90 days after it ends")
	a.checkAlerts(ctx)
	ticks, stop := a.alertTicks(alertCheckEvery)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if ctx.Err() != nil {
				return
			}
			a.checkAlerts(ctx)
		}
	}
}

// alertTicks are the loop's ticks, every apart.
func (a *API) alertTicks(every time.Duration) (<-chan time.Time, func()) {
	if a.alertLoop.ticker != nil {
		return a.alertLoop.ticker(every)
	}
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// checkAlerts is one check: it reads the status, starts and ends the alerts
// by the rules and writes the titles and texts of the active ones from the
// status — every start and every end before any mail — then makes the tries
// of their mails that are due, and sweeps the alerts past their keeping once
// an hour. A check that fails is said in the log, and the next one runs.
func (a *API) checkAlerts(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("alerts: a check stopped at an error, and the next one runs: %s", spaced(fmt.Sprint(v)))
		}
	}()
	checkTime := alertCheckTime
	if a.alertLoop.checkTime > 0 {
		checkTime = a.alertLoop.checkTime
	}
	ctx, cancel := context.WithTimeout(ctx, checkTime)
	defer cancel()
	now := a.clock()
	f, err := a.op.AlertFacts(ctx)
	switch {
	case errors.Is(err, operator.ErrNoResource):
		a.sayOnce(&a.alertLoop.noOperator, "alerts: the platform is not run by the operator, so no alert is raised")
		return
	case err != nil:
		a.sayOnce(&a.alertLoop.unreadable, "alerts: the platform's resource could not be read, so no alert starts or ends: "+spaced(err.Error()))
		return
	}
	a.alertLoop.mu.Lock()
	a.alertLoop.noOperator, a.alertLoop.unreadable = false, false
	a.alertLoop.mu.Unlock()
	a.sweepAlerts(ctx, now)
	if !f.CreatedAt.IsZero() && now.Sub(f.CreatedAt) < alertQuiet {
		a.sayOnce(&a.alertLoop.young, "alerts: the resource is younger than 1 hour, so no alert is raised yet")
		return
	}
	cctx, ccancel := callContext(ctx)
	active, err := a.alerts.ActiveAlerts(cctx)
	ccancel()
	if err != nil {
		log.Printf("alerts: the active alerts could not be read, so none starts or ends: %s", spaced(err.Error()))
		return
	}
	mine := a.startAndEnd(ctx, evaluate(f, now, active), active, now)
	a.mailAlerts(ctx, mine)
}

// startAndEnd writes what the rules say: the active alerts that hold, as the
// status has them now; the end of each that no longer holds; and the start of
// each that holds and is not active. It answers the alerts this check started
// or ended: the write that starts an alert holds its mail, and the write that
// ends one holds it where no other portal-api does, so that it mails it.
func (a *API) startAndEnd(ctx context.Context, r rules, active []model.Alert, now time.Time) []model.Alert {
	var mine []model.Alert
	for _, al := range active {
		if r.frozen[al.Kind] {
			continue
		}
		if h, ok := r.holds[alertKey{al.Kind, al.About}]; ok {
			a.rewriteAlert(ctx, al, h)
			continue
		}
		text, values := r.ending(al)
		cctx, cancel := callContext(ctx)
		ended, won, _, err := a.alerts.EndAlert(cctx, al.ID, now, text, jsonOf(values), a.holder(), alertMailHold)
		cancel()
		switch {
		case err != nil:
			log.Printf("alerts: %s about %s could not be ended: %s", al.Kind, al.About, spaced(err.Error()))
		case won:
			log.Printf("alerts: %s about %s ended", al.Kind, al.About)
			mine = append(mine, ended)
		}
	}
	keys := make([]alertKey, 0, len(r.holds))
	for k := range r.holds {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y alertKey) int {
		if c := slices.Index(alertKinds, x.kind) - slices.Index(alertKinds, y.kind); c != 0 {
			return c
		}
		return strings.Compare(x.about, y.about)
	})
	for _, k := range keys {
		h := r.holds[k]
		if h.noStart || r.frozen[k.kind] || slices.ContainsFunc(active, func(al model.Alert) bool { return al.Kind == k.kind && al.About == k.about }) {
			continue
		}
		cctx, cancel := callContext(ctx)
		al, won, err := a.alerts.StartAlert(cctx, store.NewAlert{
			Kind: k.kind, About: k.about, Title: h.title, Text: h.text, Link: alertLink(k.kind),
			Values: jsonOf(h.values), Memo: jsonOf(h.memo), At: now, Holder: a.holder(), Hold: alertMailHold,
		})
		cancel()
		switch {
		case err != nil:
			log.Printf("alerts: %s about %s could not be started: %s", k.kind, k.about, spaced(err.Error()))
		case won:
			log.Printf("alerts: %s about %s started", k.kind, k.about)
			mine = append(mine, al)
		}
	}
	return mine
}

// mailAlerts makes the tries that are due: of the alerts this check started
// or ended first, then of those whose tries no other portal-api holds. Each
// alert's mail is held right before it goes, where no other portal-api holds
// it. Where portal-api stops, or the
// check's time is nearly up, the rest are let go, to be tried at once by
// whichever portal-api runs next.
func (a *API) mailAlerts(ctx context.Context, mine []model.Alert) {
	ids := make([]string, 0, len(mine))
	for _, al := range mine {
		ids = append(ids, al.ID)
	}
	if !alertStopping(ctx) {
		cctx, cancel := callContext(ctx)
		due, err := a.alerts.DueMail(cctx, alertMailBatch)
		cancel()
		if err != nil {
			log.Printf("alerts: the tries of the alerts' mails could not be read: %s", spaced(err.Error()))
		}
		for _, id := range due {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	run := &alertRun{}
	for i, id := range ids {
		if alertStopping(ctx) {
			a.letGo(ctx, mine, ids[i:])
			return
		}
		cctx, cancel := callContext(ctx)
		al, held, err := a.alerts.LeaseMail(cctx, id, a.holder(), alertMailHold)
		cancel()
		switch {
		case err != nil:
			log.Printf("alerts: the tries of the alerts' mails could not be read: %s", spaced(err.Error()))
		case held:
			a.mailAlert(ctx, run, al)
		}
	}
}

// letGo lets go the mail of each alert of mine among ids, as it is, its tries
// due at once.
func (a *API) letGo(ctx context.Context, mine []model.Alert, ids []string) {
	now := time.Duration(0)
	for _, al := range mine {
		if !slices.Contains(ids, al.ID) {
			continue
		}
		dctx, cancel := dbContext(ctx)
		_, err := a.alerts.ReleaseMail(dctx, al.ID, a.holder(), al.Mail, &now, al.EndedAt != nil)
		cancel()
		if err != nil {
			log.Printf("alerts: what became of an alert's mail could not be kept: %s", spaced(err.Error()))
		}
	}
}

// rewriteAlert writes an active alert's title, text, values and memo as the
// status has them now, where they changed.
func (a *API) rewriteAlert(ctx context.Context, al model.Alert, h holding) {
	values, memo := jsonOf(h.values), jsonOf(h.memo)
	if al.Title == h.title && al.Text == h.text && sameJSON(al.Values, values) && sameJSON(al.Memo, memo) {
		return
	}
	cctx, cancel := callContext(ctx)
	defer cancel()
	if err := a.alerts.RewriteAlert(cctx, al.ID, h.title, h.text, values, memo); err != nil {
		log.Printf("alerts: %s about %s could not be written again: %s", al.Kind, al.About, spaced(err.Error()))
	}
}

// sweepAlerts deletes the alerts that ended more than 90 days ago, once an
// hour, a batch at a time.
func (a *API) sweepAlerts(ctx context.Context, now time.Time) {
	a.alertLoop.mu.Lock()
	due := now.Sub(a.alertLoop.swept) >= alertSweepEvery
	if due {
		a.alertLoop.swept = now
	}
	a.alertLoop.mu.Unlock()
	if !due {
		return
	}
	cctx, cancel := callContext(ctx)
	defer cancel()
	n, err := a.alerts.SweepAlerts(cctx, now.Add(-alertKept), alertSweepBatch)
	if err != nil && ctx.Err() == nil {
		log.Printf("alerts: the alerts past their keeping stay for now: %s", spaced(err.Error()))
	}
	if n > 0 {
		log.Printf("alerts: %d that ended more than 90 days ago went", n)
	}
}

// sayOnce writes line into the log where flag was not set, and sets it: a
// state the checks find again and again is said once.
func (a *API) sayOnce(flag *bool, line string) {
	a.alertLoop.mu.Lock()
	said := *flag
	*flag = true
	a.alertLoop.mu.Unlock()
	if !said {
		log.Print(line)
	}
}

// jsonOf is v as JSON, an empty object for none.
func jsonOf(v map[string]any) json.RawMessage {
	if v == nil {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// sameJSON: two JSON documents hold the same.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// ─── GET /alerts ─────────────────────────────────────────────────────────────

// alertView is an alert as GET /alerts answers it.
type alertView struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	About     string          `json:"about"`
	Title     string          `json:"title"`
	Text      string          `json:"text"`
	Values    json.RawMessage `json:"values"`
	Link      string          `json:"link"`
	StartedAt time.Time       `json:"startedAt"`
	EndedAt   *time.Time      `json:"endedAt"`
	Mail      alertMailView   `json:"mail"`
	Seen      bool            `json:"seen"`
}

// alertsDoc is GET /alerts. More is there where older alerts that ended in
// the last 90 days are kept than the answer lists.
type alertsDoc struct {
	Available bool           `json:"available"`
	Alerts    []alertView    `json:"alerts"`
	More      bool           `json:"more,omitempty"`
	Unseen    int            `json:"unseen"`
	Watched   []watchedAlert `json:"watched"`
}

// getAlerts handles GET /alerts: every active alert, the newest first, and
// the newest 500 that ended in the last 90 days, each seen or not by the
// admin who asks; more where older ones are kept; how many of those the bell
// shows they have not seen; and what is watched. Where the operator does not
// run the platform: available false, the reason no-operator, and nothing
// else.
func (a *API) getAlerts(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), a.clock()
	notRun := map[string]any{"available": false, "reason": "no-operator"}
	if a.op == nil || !a.op.Available() || a.alerts == nil {
		writeJSON(w, http.StatusOK, notRun)
		return
	}
	f, err := a.op.AlertFacts(ctx)
	switch {
	case errors.Is(err, operator.ErrNoResource):
		writeJSON(w, http.StatusOK, notRun)
		return
	case err != nil:
		log.Printf("alerts: the platform's resource could not be read: %s", spaced(err.Error()))
		status, why := unreadable(err)
		refuse(w, status, why)
		return
	}
	list, more, unseen, err := a.alerts.Alerts(ctx, caller(r), now.Add(-alertKept), now.Add(-alertInBell), alertsListed)
	if err != nil {
		serverError(w, err)
		return
	}
	views := make([]alertView, 0, len(list))
	for _, al := range list {
		values := al.Values
		if len(bytes.TrimSpace(values)) == 0 {
			values = json.RawMessage(`{}`)
		}
		views = append(views, alertView{
			ID: al.ID, Kind: al.Kind, About: al.About, Title: al.Title, Text: al.Text, Values: values, Link: al.Link,
			StartedAt: al.StartedAt, EndedAt: al.EndedAt, Mail: mailView(al.Mail, al.EndedAt != nil), Seen: al.Seen,
		})
	}
	writeJSON(w, http.StatusOK, alertsDoc{Available: true, Alerts: views, More: more, Unseen: unseen, Watched: watched(f, a.knowsSince(ctx, f, now))})
}

// knowsSince answers whether the operator says since when a part is not
// ready: it does where the resource's schema has alert settings, which come
// with that fact. The cluster's answer is kept 5 minutes, and so is a dry run
// that failed for another reason: that tells nothing, and the parts are
// watched as they are.
func (a *API) knowsSince(ctx context.Context, f *operator.Facts, now time.Time) bool {
	a.alertLoop.mu.Lock()
	k := a.alertLoop.known
	a.alertLoop.mu.Unlock()
	if k.resource == f.Resource && !k.at.IsZero() && now.Sub(k.at) < alertsKnownFor && !now.Before(k.at) {
		return k.yes
	}
	yes, err := a.op.KnowsAlerts(ctx, f.Resource)
	if err != nil {
		yes = true
	}
	a.alertLoop.mu.Lock()
	a.alertLoop.known.at, a.alertLoop.known.resource, a.alertLoop.known.yes = now, f.Resource, yes
	a.alertLoop.mu.Unlock()
	return yes
}

// ─── PATCH /alerts/settings ──────────────────────────────────────────────────

// alertsBody is what PATCH /alerts/settings takes, each field as it came.
type alertsBody struct {
	PartNotReadyMinutes json.RawMessage `json:"partNotReadyMinutes"`
	VolumeFullPercent   json.RawMessage `json:"volumeFullPercent"`
	CertificateDays     json.RawMessage `json:"certificateDays"`
	BackupAgeDays       json.RawMessage `json:"backupAgeDays"`
	Mail                json.RawMessage `json:"mail"`
}

func (b alertsBody) threshold(field string) json.RawMessage {
	switch field {
	case operator.SettingPartNotReady.Field:
		return b.PartNotReadyMinutes
	case operator.SettingVolumeFull.Field:
		return b.VolumeFullPercent
	case operator.SettingCertificate.Field:
		return b.CertificateDays
	case operator.SettingBackupAge.Field:
		return b.BackupAgeDays
	}
	return nil
}

// wholeNumber reads a whole number, false for anything else: a fraction, a
// number written as text, one too large.
func wholeNumber(raw json.RawMessage) (int, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return 0, false
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1e9 {
		return 0, false
	}
	return int(f), true
}

// patchAlertSettings handles PATCH /alerts/settings {partNotReadyMinutes,
// volumeFullPercent, certificateDays, backupAgeDays, mail}: each given, into
// spec.alerts, and the generation that write made. A threshold outside its
// range, or of the wrong kind, is refused with its range. The write validates
// strictly: where the cluster does not know alert settings it writes nothing
// and says so, as save on the mail page does.
func (a *API) patchAlertSettings(w http.ResponseWriter, r *http.Request) {
	if a.op == nil {
		refuse(w, http.StatusConflict, refuseAlertsNotRunByOperator)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAlertsBody)
	var body alertsBody
	if !decode(w, r, &body) {
		return
	}
	spec := map[string]any{}
	for _, s := range operator.AlertThresholds {
		raw := body.threshold(s.Field)
		if leftOut(raw) {
			continue
		}
		n, ok := wholeNumber(raw)
		if !ok || n < s.Least || n > s.Most {
			refuse(w, http.StatusBadRequest, refuseThreshold(s))
			return
		}
		spec[s.Field] = n
	}
	if !leftOut(body.Mail) {
		var on bool
		if json.Unmarshal(body.Mail, &on) != nil {
			badRequest(w, "mail is true or false")
			return
		}
		spec["mail"] = on
	}
	if len(spec) == 0 {
		badRequest(w, "name at least one of partNotReadyMinutes, volumeFullPercent, certificateDays, backupAgeDays and mail")
		return
	}
	ctx := r.Context()
	f, err := a.op.AlertFacts(ctx)
	switch {
	case errors.Is(err, operator.ErrNoResource):
		refuse(w, http.StatusConflict, refuseAlertsNotRunByOperator)
		return
	case err != nil:
		log.Printf("alerts: the platform's resource could not be read: %s", spaced(err.Error()))
		status, why := unreadable(err)
		refuse(w, status, why)
		return
	}
	generation, err := a.op.SetAlerts(ctx, f.Resource, spec)
	if err != nil {
		alertsWriteFailed(w, err)
		return
	}
	// Who saved, in the log: the request reaches the cluster as portal-api's
	// own service account.
	log.Printf("alerts: %s saved the alert settings", spaced(requester(r)))
	writeJSON(w, http.StatusOK, map[string]any{"generation": generation})
}

// alertsWriteFailed answers a write of the alert settings the cluster did not
// take.
func alertsWriteFailed(w http.ResponseWriter, err error) {
	var ae *k8s.APIError
	switch {
	case errors.Is(err, operator.ErrAlertsUnknown):
		log.Printf("alerts: the settings were not saved: %s", spaced(err.Error()))
		refuse(w, http.StatusServiceUnavailable, refuseAlertsUnknown.with(err.Error()))
	case k8s.IsNotFound(err):
		refuse(w, http.StatusConflict, refuseAlertsNotRunByOperator)
	case k8s.IsForbidden(err):
		refuse(w, http.StatusServiceUnavailable, refuseAlertsMayNotChange.with(err.Error()))
	case errors.As(err, &ae) && (ae.Code == http.StatusBadRequest || ae.Code == http.StatusUnprocessableEntity):
		refuse(w, http.StatusUnprocessableEntity, refuseAlertsRefused.with(err.Error()))
	default:
		refuse(w, http.StatusBadGateway, refuseClusterFailed.with(err.Error()))
	}
}

// ─── POST /alerts/{id}/read and /alerts/read-all ─────────────────────────────

// readAlert handles POST /alerts/{id}/read: the alert, seen by the admin who
// asks, and how many of those the bell shows they have not seen still. An
// alert there is not, or no longer, is 404.
func (a *API) readAlert(w http.ResponseWriter, r *http.Request) {
	if a.alerts == nil {
		http.Error(w, "no such alert", http.StatusNotFound)
		return
	}
	now := a.clock()
	unseen, err := a.alerts.SeeAlert(r.Context(), caller(r), chi.URLParam(r, "id"), now, now.Add(-alertInBell))
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such alert", http.StatusNotFound)
	case err != nil:
		serverError(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"unseen": unseen})
	}
}

// readAllAlerts handles POST /alerts/read-all: every alert the admin who asks
// had not seen since it last changed, seen by them, and how many that was.
func (a *API) readAllAlerts(w http.ResponseWriter, r *http.Request) {
	if a.alerts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"seen": 0, "unseen": 0})
		return
	}
	n, err := a.alerts.SeeAllAlerts(r.Context(), caller(r), a.clock())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seen": n, "unseen": 0})
}
