package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// fakeAlerts is the alerts table in memory, with the rules the store's
// statements keep (store/alerts.go, held to them against Postgres there): one
// active alert of a kind for one subject, one write that ends it, and an
// alert's mail held by one portal-api at a time, until its lease ends by the
// database's clock — here the test's.
type fakeAlerts struct {
	mu   sync.Mutex
	rows []*fakeAlert
	seen map[[2]string]time.Time
	n    int
	// now is the database's clock.
	now func() time.Time
	// failed: every call fails with it, where it is set; fail, where it is
	// set, says with what a call of the method named fails about an alert.
	failed error
	fail   func(method string, a model.Alert) error
	// calls counts the calls of each method.
	calls map[string]int
}

type fakeAlert struct {
	a      model.Alert
	holder string
	lease  *time.Time
}

func newFakeAlerts(now func() time.Time) *fakeAlerts {
	return &fakeAlerts{seen: map[[2]string]time.Time{}, now: now, calls: map[string]int{}}
}

// call counts a call of method about a, and answers what it fails with: as
// a database's driver does, a call whose context has ended fails.
func (f *fakeAlerts) call(ctx context.Context, method string, a model.Alert) error {
	f.calls[method]++
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failed != nil {
		return f.failed
	}
	if f.fail != nil {
		return f.fail(method, a)
	}
	return nil
}

func (f *fakeAlerts) find(id string) *fakeAlert {
	for _, r := range f.rows {
		if r.a.ID == id {
			return r
		}
	}
	return nil
}

// free: nobody holds r's mail.
func (f *fakeAlerts) free(r *fakeAlert) bool { return r.lease == nil || !r.lease.After(f.now()) }

// holds: holder holds r's mail.
func (f *fakeAlerts) holds(r *fakeAlert, holder string) bool {
	return r.holder == holder && r.lease != nil && r.lease.After(f.now())
}

func (f *fakeAlerts) take(r *fakeAlert, holder string, hold time.Duration) {
	l := f.now().Add(hold)
	r.holder, r.lease = holder, &l
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func at(t time.Time) *time.Time { return &t }

func (f *fakeAlerts) StartAlert(ctx context.Context, in store.NewAlert) (model.Alert, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "StartAlert", model.Alert{Kind: in.Kind, About: in.About}); err != nil {
		return model.Alert{}, false, err
	}
	if utf8.RuneCountInString(in.Title) > maxAlertTitle || utf8.RuneCountInString(in.Text) > maxAlertText || in.Title == "" || in.Text == "" ||
		in.About == "" || utf8.RuneCountInString(in.About) > 255 {
		return model.Alert{}, false, fmt.Errorf("alerts: check constraint: title %d, text %d", utf8.RuneCountInString(in.Title), utf8.RuneCountInString(in.Text))
	}
	for _, r := range f.rows {
		if r.a.Kind == in.Kind && r.a.About == in.About && r.a.EndedAt == nil {
			return model.Alert{}, false, nil
		}
	}
	f.n++
	r := &fakeAlert{a: model.Alert{
		ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.n), Kind: in.Kind, About: in.About, Title: in.Title, Text: in.Text,
		Values: orEmpty(in.Values), Link: in.Link, Memo: orEmpty(in.Memo), StartedAt: in.At, Mail: json.RawMessage(`{}`), MailNext: at(f.now()),
	}}
	f.take(r, in.Holder, in.Hold)
	f.rows = append(f.rows, r)
	return r.a, true, nil
}

func (f *fakeAlerts) ActiveAlerts(ctx context.Context) ([]model.Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "ActiveAlerts", model.Alert{}); err != nil {
		return nil, err
	}
	var out []model.Alert
	for _, r := range f.rows {
		if r.a.EndedAt == nil {
			out = append(out, r.a)
		}
	}
	slices.SortStableFunc(out, func(x, y model.Alert) int { return x.StartedAt.Compare(y.StartedAt) })
	return out, nil
}

func (f *fakeAlerts) RewriteAlert(ctx context.Context, id, title, text string, values, memo json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.find(id)
	var a model.Alert
	if r != nil {
		a = r.a
	}
	if err := f.call(ctx, "RewriteAlert", a); err != nil {
		return err
	}
	if r != nil && r.a.EndedAt == nil {
		if utf8.RuneCountInString(title) > maxAlertTitle || utf8.RuneCountInString(text) > maxAlertText {
			return errors.New("alerts: check constraint")
		}
		r.a.Title, r.a.Text, r.a.Values, r.a.Memo = title, text, orEmpty(values), orEmpty(memo)
	}
	return nil
}

func (f *fakeAlerts) EndAlert(ctx context.Context, id string, ended time.Time, text string, values json.RawMessage, holder string, hold time.Duration) (model.Alert, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.find(id)
	var a model.Alert
	if r != nil {
		a = r.a
	}
	if err := f.call(ctx, "EndAlert", a); err != nil {
		return model.Alert{}, false, false, err
	}
	if r == nil || r.a.EndedAt != nil {
		return model.Alert{}, false, false, nil
	}
	if utf8.RuneCountInString(text) > maxAlertText || text == "" {
		return model.Alert{}, false, false, errors.New("alerts: check constraint: end_text")
	}
	if ended.Before(r.a.StartedAt) {
		ended = r.a.StartedAt
	}
	r.a.EndedAt, r.a.EndText, r.a.EndValues, r.a.MailNext = &ended, text, orEmpty(values), at(f.now())
	if f.free(r) {
		f.take(r, holder, hold)
	}
	return r.a, true, f.holds(r, holder), nil
}

func (f *fakeAlerts) StartMailedWithin(ctx context.Context, kind, about string, within time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "StartMailedWithin", model.Alert{Kind: kind, About: about}); err != nil {
		return false, err
	}
	since := f.now().Add(-within)
	for _, r := range f.rows {
		if r.a.Kind == kind && r.a.About == about && r.a.StartMailedAt != nil && r.a.StartMailedAt.After(since) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAlerts) DueMail(ctx context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "DueMail", model.Alert{}); err != nil {
		return nil, err
	}
	var due []*fakeAlert
	for _, r := range f.rows {
		if r.a.MailNext != nil && !r.a.MailNext.After(f.now()) && f.free(r) {
			due = append(due, r)
		}
	}
	slices.SortStableFunc(due, func(x, y *fakeAlert) int {
		if c := x.a.MailNext.Compare(*y.a.MailNext); c != 0 {
			return c
		}
		return strings.Compare(x.a.ID, y.a.ID)
	})
	var out []string
	for i, r := range due {
		if i >= limit {
			break
		}
		out = append(out, r.a.ID)
	}
	return out, nil
}

func (f *fakeAlerts) LeaseMail(ctx context.Context, id, holder string, hold time.Duration) (model.Alert, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.find(id)
	var a model.Alert
	if r != nil {
		a = r.a
	}
	if err := f.call(ctx, "LeaseMail", a); err != nil {
		return model.Alert{}, false, err
	}
	if r == nil || r.a.MailNext == nil || r.a.MailNext.After(f.now()) || !f.free(r) && r.holder != holder {
		return model.Alert{}, false, nil
	}
	f.take(r, holder, hold)
	return r.a, true, nil
}

func (f *fakeAlerts) KeepMail(ctx context.Context, id, holder string, mail json.RawMessage, startMailed bool, hold time.Duration) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.find(id)
	var a model.Alert
	if r != nil {
		a = r.a
	}
	if err := f.call(ctx, "KeepMail", a); err != nil {
		return false, false, err
	}
	if r == nil || !f.holds(r, holder) {
		return false, false, nil
	}
	r.a.Mail = orEmpty(mail)
	f.take(r, holder, hold)
	if startMailed && r.a.StartMailedAt == nil {
		r.a.StartMailedAt = at(f.now())
	}
	return true, r.a.EndedAt != nil, nil
}

func (f *fakeAlerts) ReleaseMail(ctx context.Context, id, holder string, mail json.RawMessage, next *time.Duration, sawEnded bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.find(id)
	var a model.Alert
	if r != nil {
		a = r.a
	}
	if err := f.call(ctx, "ReleaseMail", a); err != nil {
		return false, err
	}
	if r == nil || !f.holds(r, holder) {
		return false, nil
	}
	r.a.Mail, r.holder, r.lease = orEmpty(mail), "", nil
	switch {
	case r.a.EndedAt != nil && !sawEnded:
		// Ended meanwhile by another portal-api: its end mail is due.
		r.a.MailNext = at(f.now())
	case next == nil:
		r.a.MailNext = nil
	default:
		r.a.MailNext = at(f.now().Add(*next))
	}
	return true, nil
}

// seenSince: the admin sub has seen r since it last changed.
func (f *fakeAlerts) seenSince(r *fakeAlert, sub string) bool {
	seenAt, ok := f.seen[[2]string{r.a.ID, sub}]
	changed := r.a.StartedAt
	if r.a.EndedAt != nil {
		changed = *r.a.EndedAt
	}
	return ok && !seenAt.Before(changed)
}

func (f *fakeAlerts) unseen(sub string, bellSince time.Time) int {
	n := 0
	for _, r := range f.rows {
		if (r.a.EndedAt == nil || !r.a.EndedAt.Before(bellSince)) && !f.seenSince(r, sub) {
			n++
		}
	}
	return n
}

func (f *fakeAlerts) Alerts(ctx context.Context, sub string, since, bellSince time.Time, limit int) ([]model.Alert, bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "Alerts", model.Alert{}); err != nil {
		return nil, false, 0, err
	}
	var active, ended []model.Alert
	for _, r := range f.rows {
		a := r.a
		a.Seen = f.seenSince(r, sub)
		switch {
		case a.EndedAt == nil:
			active = append(active, a)
		case !a.EndedAt.Before(since):
			ended = append(ended, a)
		}
	}
	slices.SortStableFunc(active, func(x, y model.Alert) int {
		if c := y.StartedAt.Compare(x.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	slices.SortStableFunc(ended, func(x, y model.Alert) int {
		if c := y.EndedAt.Compare(*x.EndedAt); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	more := len(ended) > limit
	if more {
		ended = ended[:limit]
	}
	return append(active, ended...), more, f.unseen(sub, bellSince), nil
}

func (f *fakeAlerts) SeeAlert(ctx context.Context, sub, id string, seenAt, bellSince time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "SeeAlert", model.Alert{ID: id}); err != nil {
		return 0, err
	}
	if f.find(id) == nil {
		return 0, store.ErrNotFound
	}
	key := [2]string{id, sub}
	if prev, ok := f.seen[key]; !ok || seenAt.After(prev) {
		f.seen[key] = seenAt
	}
	return f.unseen(sub, bellSince), nil
}

func (f *fakeAlerts) SeeAllAlerts(ctx context.Context, sub string, seenAt time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "SeeAllAlerts", model.Alert{}); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range f.rows {
		if f.seenSince(r, sub) {
			continue
		}
		n++
		key := [2]string{r.a.ID, sub}
		if prev, ok := f.seen[key]; !ok || seenAt.After(prev) {
			f.seen[key] = seenAt
		}
	}
	return n, nil
}

func (f *fakeAlerts) SweepAlerts(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(ctx, "SweepAlerts", model.Alert{}); err != nil {
		return 0, err
	}
	n := 0
	for {
		gone := 0
		kept := f.rows[:0:0]
		for _, r := range f.rows {
			if gone < batch && r.a.EndedAt != nil && r.a.EndedAt.Before(cutoff) {
				gone++
				for k := range f.seen {
					if k[0] == r.a.ID {
						delete(f.seen, k)
					}
				}
				continue
			}
			kept = append(kept, r)
		}
		f.rows = kept
		f.calls["SweepAlerts batch"]++
		n += gone
		if gone < batch {
			return n, nil
		}
	}
}

// all are every alert the table holds, the oldest first.
func (f *fakeAlerts) all() []model.Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Alert, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r.a)
	}
	return out
}

// heldBy is who holds an alert's mail, "" for nobody.
func (f *fakeAlerts) heldBy(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.find(id); r != nil && !f.free(r) {
		return r.holder
	}
	return ""
}

// callsOf is how many times method was called.
func (f *fakeAlerts) callsOf(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}
