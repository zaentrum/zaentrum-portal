package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ─── First-run setup ─────────────────────────────────────────────────────────

// SetupCompletion is the record that first-run setup was marked done; nil
// while it is not.
func (s *Store) SetupCompletion(ctx context.Context) (*model.SetupCompletion, error) {
	var c model.SetupCompletion
	err := s.pool.QueryRow(ctx, `SELECT completed_at, completed_by FROM setup_completion WHERE id`).Scan(&c.At, &c.By)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CompleteSetup marks first-run setup done by by, and answers the record. Done
// already, it stays as it was: the first admin to mark it is the one it names.
func (s *Store) CompleteSetup(ctx context.Context, by string) (model.SetupCompletion, error) {
	var c model.SetupCompletion
	err := s.pool.QueryRow(ctx, `
		INSERT INTO setup_completion (id, completed_by) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET id = setup_completion.id
		RETURNING completed_at, completed_by`, by).Scan(&c.At, &c.By)
	return c, err
}

// ReopenSetup removes the record, so the checklist shows again. Not done, it
// changes nothing.
func (s *Store) ReopenSetup(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM setup_completion`)
	return err
}
