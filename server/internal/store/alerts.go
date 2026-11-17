package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ─── Alerts ──────────────────────────────────────────────────────────────────
//
// What is wrong with the platform, for its admins (migration 016). portal-api
// starts and ends the alerts from the platform's status; one alert of a kind
// is active for one subject, and the database holds that: of two portal-apis
// that start the same alert at once, one insert wins, and of two that end it,
// one update. The one whose write won takes its mail.
//
// An alert's mail is held by one portal-api at a time, the holder, until its
// lease ends. The lease is taken and kept by the database's clock, so that
// two portal-apis whose clocks differ agree on it, and every write of the
// mail names its holder: a portal-api that no longer holds an alert's mail
// writes nothing of it. The holder keeps the lease with each write.

// alertID is an alert's id as it is written: a UUID, lower case. Anything
// else names no alert, and never reaches the database as one.
var alertID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewAlert is an alert as it starts.
type NewAlert struct {
	Kind, About, Title, Text, Link string
	Values, Memo                   json.RawMessage
	// At is when it starts.
	At time.Time
	// Holder is the portal-api that starts it, which holds its mail for
	// Hold, by the database's clock; the mail is due at once.
	Holder string
	Hold   time.Duration
}

const alertCols = `id::text, kind, about, title, body, vals, link, memo, started_at, ended_at, end_text, end_vals, mail, mail_next, start_mailed_at`

// scanAlert reads an alert, its times in UTC.
func scanAlert(r rowScanner) (model.Alert, error) {
	var (
		a                     model.Alert
		vals, memo, mail, end []byte
		endText               *string
	)
	err := r.Scan(&a.ID, &a.Kind, &a.About, &a.Title, &a.Text, &vals, &a.Link, &memo, &a.StartedAt, &a.EndedAt,
		&endText, &end, &mail, &a.MailNext, &a.StartMailedAt)
	a.Values, a.Memo, a.Mail = json.RawMessage(vals), json.RawMessage(memo), json.RawMessage(mail)
	if end != nil {
		a.EndValues = json.RawMessage(end)
	}
	if endText != nil {
		a.EndText = *endText
	}
	a.StartedAt = a.StartedAt.UTC()
	for _, t := range []**time.Time{&a.EndedAt, &a.MailNext, &a.StartMailedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return a, err
}

// jsonOr is raw, or an empty object for none.
func jsonOr(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// millis is a duration as the statements take it: whole milliseconds.
func millis(d time.Duration) int64 { return d.Milliseconds() }

// StartAlert starts an alert, its mail due at once and held by in.Holder. It
// answers the alert and true where this insert started it, and false where an
// alert of that kind is active for that subject already: another portal-api
// started it, and mails it.
func (s *Store) StartAlert(ctx context.Context, in NewAlert) (model.Alert, bool, error) {
	a, err := scanAlert(s.pool.QueryRow(ctx, `
		INSERT INTO alerts (kind, about, title, body, vals, link, memo, started_at, mail_next, mail_holder, mail_lease)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::jsonb, $8, now(), $9, now() + $10 * interval '1 millisecond')
		ON CONFLICT (kind, about) WHERE ended_at IS NULL DO NOTHING
		RETURNING `+alertCols,
		in.Kind, in.About, in.Title, in.Text, jsonOr(in.Values), in.Link, jsonOr(in.Memo), in.At, in.Holder, millis(in.Hold)))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Alert{}, false, nil
	}
	if err != nil {
		return model.Alert{}, false, err
	}
	return a, true, nil
}

// ActiveAlerts are the alerts that have not ended, the oldest first.
func (s *Store) ActiveAlerts(ctx context.Context) ([]model.Alert, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+alertCols+` FROM alerts WHERE ended_at IS NULL ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.Alert, error) { return scanAlert(r) })
}

// RewriteAlert writes an active alert's title, text, values and memo as the
// status has them now. An alert that has ended keeps its last.
func (s *Store) RewriteAlert(ctx context.Context, id, title, text string, values, memo json.RawMessage) error {
	if !alertID.MatchString(id) {
		return ErrNotFound
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE alerts SET title = $2, body = $3, vals = $4::jsonb, memo = $5::jsonb
		WHERE id = $1::uuid AND ended_at IS NULL`, id, title, text, jsonOr(values), jsonOr(memo))
	return err
}

// EndAlert ends an active alert at at, with the sentence of its end, and has
// its mail due at once. It answers the alert, whether this write ended it —
// false where it had ended already — and whether holder now holds its mail:
// it does unless another portal-api holds it still, which then leaves the end
// mail to whoever takes the tries next.
func (s *Store) EndAlert(ctx context.Context, id string, at time.Time, text string, values json.RawMessage, holder string, hold time.Duration) (model.Alert, bool, bool, error) {
	if !alertID.MatchString(id) {
		return model.Alert{}, false, false, ErrNotFound
	}
	var held bool
	// Every SET reads the row as it was: the lease is taken where it was free.
	row := s.pool.QueryRow(ctx, `
		UPDATE alerts SET ended_at = GREATEST($2, started_at), end_text = $3, end_vals = $4::jsonb, mail_next = now(),
			mail_holder = CASE WHEN mail_lease IS NULL OR mail_lease <= now() THEN $5 ELSE mail_holder END,
			mail_lease = CASE WHEN mail_lease IS NULL OR mail_lease <= now() THEN now() + $6 * interval '1 millisecond' ELSE mail_lease END
		WHERE id = $1::uuid AND ended_at IS NULL
		RETURNING mail_holder IS NOT DISTINCT FROM $5 AND mail_lease > now(), `+alertCols, id, at, text, jsonOr(values), holder, millis(hold))
	a, err := scanAlert(firstScanner{row: row, first: &held})
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Alert{}, false, false, nil
	}
	if err != nil {
		return model.Alert{}, false, false, err
	}
	return a, true, held, nil
}

// firstScanner reads one value before the alert: whether a write took its
// lease.
type firstScanner struct {
	row   pgx.Row
	first *bool
}

func (l firstScanner) Scan(dest ...any) error {
	return l.row.Scan(append([]any{l.first}, dest...)...)
}

// StartMailedWithin answers whether the mail server took the start mail of an
// alert of that kind about that subject less than within ago, by the
// database's clock: the 24 hours in which one start mail is sent count from
// when it first took the mail of the last start.
func (s *Store) StartMailedWithin(ctx context.Context, kind, about string, within time.Duration) (bool, error) {
	var yes bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM alerts WHERE kind = $1 AND about = $2 AND start_mailed_at > now() - $3 * interval '1 millisecond')`,
		kind, about, millis(within)).Scan(&yes)
	return yes, err
}

// DueMail are the alerts whose mail is due and that no portal-api holds, the
// longest due first, at most limit of them.
func (s *Store) DueMail(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM alerts
		WHERE mail_next <= now() AND (mail_lease IS NULL OR mail_lease <= now())
		ORDER BY mail_next, id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// LeaseMail has holder hold an alert's mail for hold, right before it goes:
// where it is due and nobody else holds it, or holder does already. It
// answers the alert as it is now, its mail with it, and whether holder holds
// it.
func (s *Store) LeaseMail(ctx context.Context, id, holder string, hold time.Duration) (model.Alert, bool, error) {
	if !alertID.MatchString(id) {
		return model.Alert{}, false, ErrNotFound
	}
	a, err := scanAlert(s.pool.QueryRow(ctx, `
		UPDATE alerts SET mail_holder = $2, mail_lease = now() + $3 * interval '1 millisecond'
		WHERE id = $1::uuid AND mail_next IS NOT NULL AND mail_next <= now()
			AND (mail_lease IS NULL OR mail_lease <= now() OR mail_holder = $2)
		RETURNING `+alertCols, id, holder, millis(hold)))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Alert{}, false, nil
	}
	if err != nil {
		return model.Alert{}, false, err
	}
	return a, true, nil
}

// KeepMail writes what became of an alert's mail while holder holds it, and
// keeps the lease for hold; with startMailed, the mail server has taken the
// start mail for an address, and the first time it did is kept. held is false
// where holder no longer holds it: then nothing is written. ended says
// whether the alert has ended, by whichever portal-api ended it.
func (s *Store) KeepMail(ctx context.Context, id, holder string, mail json.RawMessage, startMailed bool, hold time.Duration) (held, ended bool, err error) {
	if !alertID.MatchString(id) {
		return false, false, ErrNotFound
	}
	err = s.pool.QueryRow(ctx, `
		UPDATE alerts SET mail = $3::jsonb, mail_lease = now() + $5 * interval '1 millisecond',
			start_mailed_at = CASE WHEN $4 THEN COALESCE(start_mailed_at, now()) ELSE start_mailed_at END
		WHERE id = $1::uuid AND mail_holder = $2 AND mail_lease > now()
		RETURNING ended_at IS NOT NULL`, id, holder, jsonOr(mail), startMailed, millis(hold)).Scan(&ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, ended, nil
}

// ReleaseMail writes what became of an alert's mail while holder holds it,
// and lets it go: the next try is due after next, nil for none. sawEnded says
// whether holder carried the mail on knowing the alert had ended: where
// another portal-api ended it meanwhile, its end mail is due at once. false
// where holder no longer holds it: then nothing is written.
func (s *Store) ReleaseMail(ctx context.Context, id, holder string, mail json.RawMessage, next *time.Duration, sawEnded bool) (bool, error) {
	if !alertID.MatchString(id) {
		return false, ErrNotFound
	}
	var after *int64
	if next != nil {
		ms := millis(*next)
		after = &ms
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE alerts SET mail = $3::jsonb, mail_holder = NULL, mail_lease = NULL,
			mail_next = CASE WHEN ended_at IS NOT NULL AND NOT $5 THEN now()
				WHEN $4::bigint IS NULL THEN NULL
				ELSE now() + $4::bigint * interval '1 millisecond' END
		WHERE id = $1::uuid AND mail_holder = $2 AND mail_lease > now()`, id, holder, jsonOr(mail), after, sawEnded)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// seenCond is the condition that an alert a, as alertsFrom joins it, was seen
// by the admin since it last changed: when it started, or when it ended.
const seenCond = `seen.seen_at IS NOT NULL AND seen.seen_at >= COALESCE(a.ended_at, a.started_at)`

// alertsFrom joins each alert with the seen mark of the admin $1.
const alertsFrom = `alerts a LEFT JOIN alert_seen seen ON seen.alert_id = a.id AND seen.sub = $1`

// Alerts are every active alert, the newest first, and the newest limit that
// ended at since or later, each seen or not by sub; more says that older ones
// that ended since are kept. unseen is how many of the alerts the bell shows
// — the active ones, and those that ended at bellSince or later — sub has not
// seen since they last changed.
func (s *Store) Alerts(ctx context.Context, sub string, since, bellSince time.Time, limit int) ([]model.Alert, bool, int, error) {
	read := func(sql string, args ...any) ([]model.Alert, error) {
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.Alert, error) {
			var seen bool
			a, err := scanAlert(seenScanner{row: r, seen: &seen})
			a.Seen = seen
			return a, err
		})
	}
	cols := prefixed("a.", alertCols) + `, ` + seenCond
	active, err := read(`SELECT `+cols+` FROM `+alertsFrom+` WHERE a.ended_at IS NULL ORDER BY a.started_at DESC, a.id`, sub)
	if err != nil {
		return nil, false, 0, err
	}
	ended, err := read(`SELECT `+cols+` FROM `+alertsFrom+` WHERE a.ended_at >= $2 ORDER BY a.ended_at DESC, a.id LIMIT $3`, sub, since, limit+1)
	if err != nil {
		return nil, false, 0, err
	}
	more := len(ended) > limit
	if more {
		ended = ended[:limit]
	}
	unseen, err := s.unseenAlerts(ctx, sub, bellSince)
	return append(active, ended...), more, unseen, err
}

// seenScanner reads an alert, and then whether it was seen.
type seenScanner struct {
	row  rowScanner
	seen *bool
}

func (l seenScanner) Scan(dest ...any) error {
	return l.row.Scan(append(dest, l.seen)...)
}

// prefixed is a column list with each column of the alerts named through
// prefix.
func prefixed(prefix, cols string) string {
	return prefix + strings.ReplaceAll(cols, ", ", ", "+prefix)
}

func (s *Store) unseenAlerts(ctx context.Context, sub string, bellSince time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM `+alertsFrom+`
		WHERE (a.ended_at IS NULL OR a.ended_at >= $2) AND NOT (`+seenCond+`)`, sub, bellSince).Scan(&n)
	return n, err
}

// SeeAlert has sub see the alert at at, and answers how many of the alerts
// the bell shows they have not seen since. ErrNotFound when there is no
// alert of that id.
func (s *Store) SeeAlert(ctx context.Context, sub, id string, at, bellSince time.Time) (int, error) {
	if !alertID.MatchString(id) {
		return 0, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO alert_seen (alert_id, sub, seen_at)
		SELECT id, $2, $3 FROM alerts WHERE id = $1::uuid
		ON CONFLICT (alert_id, sub) DO UPDATE SET seen_at = GREATEST(alert_seen.seen_at, EXCLUDED.seen_at)`, id, sub, at)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	return s.unseenAlerts(ctx, sub, bellSince)
}

// SeeAllAlerts has sub see, at at, every alert they had not seen since it
// last changed, and answers how many: it marks those and no other.
func (s *Store) SeeAllAlerts(ctx context.Context, sub string, at time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO alert_seen (alert_id, sub, seen_at)
		SELECT a.id, $1, $2 FROM `+alertsFrom+` WHERE NOT (`+seenCond+`)
		ON CONFLICT (alert_id, sub) DO UPDATE SET seen_at = GREATEST(alert_seen.seen_at, EXCLUDED.seen_at)`, sub, at)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// SweepAlerts deletes the alerts that ended before cutoff, batch at a time,
// and answers how many. An active alert is kept, however old.
func (s *Store) SweepAlerts(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	total := 0
	for {
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM alerts WHERE id IN (
				SELECT id FROM alerts WHERE ended_at IS NOT NULL AND ended_at < $1 ORDER BY ended_at, id LIMIT $2)`, cutoff, batch)
		if err != nil {
			return total, err
		}
		n := int(tag.RowsAffected())
		total += n
		if n < batch {
			return total, nil
		}
	}
}
