// Package store is the Postgres data-access layer for the portal registry
// (apps / spaces / tiles).
package store

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a pool against the given DSN. user/password override when non-empty.
func New(ctx context.Context, dsn, user, password string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if user != "" {
		cfg.ConnConfig.User = user
	}
	if password != "" {
		cfg.ConnConfig.Password = password
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping verifies connectivity (used by the readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies every migrations/*.sql from fsys in lexical order. All
// statements are idempotent, so this is safe to run on every boot.
func (s *Store) Migrate(ctx context.Context, fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := fs.ReadFile(fsys, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := s.pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		log.Printf("migrations: applied %s", name)
	}
	return nil
}

// ApplyChinoPublicURL upgrades the chino app's SEED-DEFAULT base_url
// ("/chino/") to the deployment's public URL — a subdomain-routed instance
// (e.g. https://chino.beta.nalet.cloud/) wants the tile to open the real
// origin, not the portal-host path. Guarded on the seed default so a value an
// admin edited in the registry console is never clobbered (same contract as
// the tile open_mode default). No-op when url is blank or already applied.
func (s *Store) ApplyChinoPublicURL(ctx context.Context, url string) error {
	if url == "" {
		return nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE apps SET base_url = $1 WHERE key = 'chino' AND base_url = '/chino/'`, url)
	if err != nil {
		return fmt.Errorf("apply chino public url: %w", err)
	}
	if tag.RowsAffected() > 0 {
		log.Printf("registry: chino base_url -> %s (seed default upgraded)", url)
	}
	return nil
}
