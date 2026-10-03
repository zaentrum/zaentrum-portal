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

// migrationLock is the session advisory lock the migrations run under.
const migrationLock int64 = 0x7a61656e7472756d // "zaentrum"

// Migrate applies every migrations/*.sql from fsys in lexical order. All
// statements are idempotent, so this is safe to run on every boot.
//
// They run on one connection that holds an advisory lock — replicas booting
// together take turns, so a step that checks before it changes, like the
// audience backfill in 001, runs once — and that carries the realm's admin
// role as zaentrum.admin_role, the role the seed makes its admin tiles
// visible to.
func (s *Store) Migrate(ctx context.Context, fsys fs.FS, adminRole string) error {
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

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("migrations: lock: %w", err)
	}
	defer func() {
		// Background: the lock goes with the session if this fails, but a
		// cancelled boot must not leave it held on a pooled connection.
		_, _ = conn.Exec(context.Background(), `SELECT set_config('zaentrum.admin_role', '', false), pg_advisory_unlock($1)`, migrationLock)
	}()
	if _, err := conn.Exec(ctx, `SELECT set_config('zaentrum.admin_role', $1, false)`, adminRole); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	for _, name := range names {
		body, err := fs.ReadFile(fsys, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		log.Printf("migrations: applied %s", name)
	}
	return nil
}

// ApplyChinoPublicURL upgrades the chino app's SEED-DEFAULT base_url
// ("/chino/") to the deployment's public URL — a subdomain-routed instance
// (e.g. https://chino.example.com/) wants the tile to open the real
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
