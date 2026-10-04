package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ─── Invites ─────────────────────────────────────────────────────────────────
//
// An invite is the SHA-256 of its token, whose account it is for, and what
// became of it (migration 014). The token itself is never here.

// ErrInviteClosed: no invite opens for that token — none of that hash, or one
// used, revoked or expired. The same error for every one of them.
var ErrInviteClosed = errors.New("no open invite for that token")

// inviteKept is how long a closed invite's row is kept, for the People page
// to say what became of it.
const inviteKept = 90 * 24 * time.Hour

const inviteCols = `id, token_hash, user_id, created_by, created_at, expires_at, used_at, revoked_at`

func scanInvite(row pgx.Row) (model.Invite, error) {
	var i model.Invite
	err := row.Scan(&i.ID, &i.TokenHash, &i.UserID, &i.CreatedBy, &i.CreatedAt, &i.ExpiresAt, &i.UsedAt, &i.RevokedAt)
	return i, err
}

// CreateInvite records an invite for the account userID, made by createdBy,
// open until expires; the account's invites still open are revoked. Closed
// invites older than inviteKept go.
func (s *Store) CreateInvite(ctx context.Context, userID, createdBy string, hash []byte, expires time.Time) (model.Invite, error) {
	if len(hash) != 32 || userID == "" {
		return model.Invite{}, fmt.Errorf("an invite needs a SHA-256 and an account")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Invite{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE invites SET revoked_at = now()
		WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, userID); err != nil {
		return model.Invite{}, err
	}
	inv, err := scanInvite(tx.QueryRow(ctx, `
		INSERT INTO invites (token_hash, user_id, created_by, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+inviteCols, hash, userID, createdBy, expires))
	if err != nil {
		return model.Invite{}, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM invites
		WHERE (used_at IS NOT NULL OR revoked_at IS NOT NULL OR expires_at < now())
		  AND created_at < now() - make_interval(secs => $1)`, inviteKept.Seconds()); err != nil {
		return model.Invite{}, err
	}
	return inv, tx.Commit(ctx)
}

// LatestInvites is each account's newest invite, by account.
func (s *Store) LatestInvites(ctx context.Context) (map[string]model.Invite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (user_id) `+inviteCols+`
		FROM invites ORDER BY user_id, created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.Invite{}
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out[inv.UserID] = inv
	}
	return out, rows.Err()
}

// OpenInvite is the invite that opens for a token of SHA-256 hash at now, or
// ErrInviteClosed.
func (s *Store) OpenInvite(ctx context.Context, hash []byte, now time.Time) (model.Invite, error) {
	inv, err := scanInvite(s.pool.QueryRow(ctx, `SELECT `+inviteCols+` FROM invites WHERE token_hash = $1`, hash))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The same comparison for a token of no invite as for one of an
		// invite, so that neither answers sooner.
		(&model.Invite{TokenHash: make([]byte, 32)}).Opens(hash, now)
		return model.Invite{}, ErrInviteClosed
	case err != nil:
		return model.Invite{}, err
	case !inv.Opens(hash, now):
		return model.Invite{}, ErrInviteClosed
	}
	return inv, nil
}

// UseInvite uses the invite that opens for a token of SHA-256 hash, once: it
// locks the row, calls use with it, and marks it used only when use succeeds
// — a password the realm refused leaves the invite open. A second request
// with the same token waits for the first, then finds the invite used:
// ErrInviteClosed, as for one that never opened.
func (s *Store) UseInvite(ctx context.Context, hash []byte, now time.Time, use func(model.Invite) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inv, err := scanInvite(tx.QueryRow(ctx, `SELECT `+inviteCols+` FROM invites WHERE token_hash = $1 FOR UPDATE`, hash))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		(&model.Invite{TokenHash: make([]byte, 32)}).Opens(hash, now)
		return ErrInviteClosed
	case err != nil:
		return err
	case !inv.Opens(hash, now):
		return ErrInviteClosed
	}
	if err := use(inv); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET used_at = $2 WHERE id = $1`, inv.ID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeInvites revokes the account's invites still open.
func (s *Store) RevokeInvites(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE invites SET revoked_at = now()
		WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, userID)
	return err
}

// DeleteInvites removes every invite of the account.
func (s *Store) DeleteInvites(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM invites WHERE user_id = $1`, userID)
	return err
}
