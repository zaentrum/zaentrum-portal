package store

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ─── Notices ─────────────────────────────────────────────────────────────────
//
// What an addon tells one person (migration 015). A row is its person's —
// every read and change names the subject it is for, and finds nothing of
// anyone else's — and its addon's: removing the addon removes its rows.

// NoticesKept is how many notices a person keeps: their newest. An older one
// goes when a new one comes.
const NoticesKept = 100

// noticeLock is the namespace of the advisory lock a person's new notice is
// written under, so that two arriving at once keep the cap between them.
const noticeLock int32 = 0x6e6f7469 // "noti"

// ErrNoAddon: the addon a notice names is not installed.
var ErrNoAddon = errors.New("no addon of that key is installed")

// noticeID is a notice's id as it is written: a UUID, lower case. Anything
// else names no notice, and never reaches the database as one.
var noticeID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewNotice is a notice as it is posted: for whom, from which addon, and
// what it says — checked by the API before it gets here.
type NewNotice struct {
	Sub    string
	Addon  string
	Title  string
	Body   string
	Link   string
	ItemID string
}

const noticeCols = `n.id::text, n.addon, a.title, a.icon, n.title, n.body, n.link, n.item_id, n.created_at, n.read_at`

// noticeFrom joins the addon's app, which says what the notice is from. An
// addon's key is its app's (008), so every notice has one.
const noticeFrom = `notices n JOIN apps a ON a.key = n.addon`

func scanNotice(r rowScanner) (model.Notice, error) {
	var n model.Notice
	err := r.Scan(&n.ID, &n.Addon, &n.AddonTitle, &n.AddonIcon, &n.Title, &n.Body, &n.Link, &n.ItemID, &n.CreatedAt, &n.ReadAt)
	return n, err
}

// PostNotice stores a notice for its person and keeps their newest
// NoticesKept: it answers the notice as stored and how many older ones went.
// ErrNoAddon when the addon is not installed.
func (s *Store) PostNotice(ctx context.Context, in NewNotice) (model.Notice, int, error) {
	var (
		out    model.Notice
		pruned int
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// One person's notices are written one at a time, and dated once the
		// lock is held, so that the newest is always the last one written.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, noticeLock, in.Sub); err != nil {
			return err
		}
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO notices (sub, addon, title, body, link, item_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp())
			RETURNING id::text`, in.Sub, in.Addon, in.Title, in.Body, in.Link, in.ItemID).Scan(&id)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
			return ErrNoAddon
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM notices WHERE id IN (
				SELECT id FROM notices WHERE sub = $1
				ORDER BY created_at DESC, id DESC OFFSET $2)`, in.Sub, NoticesKept)
		if err != nil {
			return err
		}
		pruned = int(tag.RowsAffected())
		out, err = scanNotice(tx.QueryRow(ctx, `SELECT `+noticeCols+` FROM `+noticeFrom+` WHERE n.id = $1::uuid`, id))
		return err
	})
	return out, pruned, err
}

// Notices is the person's notices, newest first, at most limit of them, and
// how many of all of theirs are unread.
func (s *Store) Notices(ctx context.Context, sub string, limit int) ([]model.Notice, int, error) {
	if limit <= 0 || limit > NoticesKept {
		limit = NoticesKept
	}
	rows, err := s.pool.Query(ctx, `SELECT `+noticeCols+` FROM `+noticeFrom+`
		WHERE n.sub = $1 ORDER BY n.created_at DESC, n.id DESC LIMIT $2`, sub, limit)
	if err != nil {
		return nil, 0, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.Notice, error) { return scanNotice(r) })
	if err != nil {
		return nil, 0, err
	}
	unread, err := s.unreadNotices(ctx, sub)
	return out, unread, err
}

func (s *Store) unreadNotices(ctx context.Context, sub string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notices WHERE sub = $1 AND read_at IS NULL`, sub).Scan(&n)
	return n, err
}

// ReadNotice marks the person's notice read — once: reading it again keeps
// when it was first read — and answers how many of theirs are unread still.
// ErrNotFound when they have no notice of that id.
func (s *Store) ReadNotice(ctx context.Context, sub, id string) (int, error) {
	if !noticeID.MatchString(id) {
		return 0, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE notices SET read_at = coalesce(read_at, now())
		WHERE sub = $1 AND id = $2::uuid`, sub, id)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	return s.unreadNotices(ctx, sub)
}

// ReadAllNotices marks every unread notice of the person read, and answers
// how many it marked.
func (s *Store) ReadAllNotices(ctx context.Context, sub string) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE notices SET read_at = now() WHERE sub = $1 AND read_at IS NULL`, sub)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DeleteNotice deletes the person's notice; ErrNotFound when they have no
// notice of that id.
func (s *Store) DeleteNotice(ctx context.Context, sub, id string) error {
	if !noticeID.MatchString(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM notices WHERE sub = $1 AND id = $2::uuid`, sub, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePersonNotices deletes every notice of the person, and answers how
// many there were.
func (s *Store) DeletePersonNotices(ctx context.Context, sub string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notices WHERE sub = $1`, sub)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// SweepNotices deletes every notice posted before cutoff, and answers how
// many.
func (s *Store) SweepNotices(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notices WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// NoticeCounts counts each addon's notices — how many, how many unread, for
// how many people, the newest — never what they say. addon "" counts every
// addon that has any; an addon named that has none has no row.
func (s *Store) NoticeCounts(ctx context.Context, addon string) ([]model.NoticeCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT addon, count(*), count(*) FILTER (WHERE read_at IS NULL), count(DISTINCT sub), max(created_at)
		FROM notices WHERE $1 = '' OR addon = $1
		GROUP BY addon ORDER BY addon`, addon)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.NoticeCount, error) {
		var c model.NoticeCount
		err := r.Scan(&c.Addon, &c.Notices, &c.Unread, &c.People, &c.Latest)
		return c, err
	})
}
