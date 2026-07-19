package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ErrNotFound is returned when a keyed row does not exist.
var ErrNotFound = errors.New("not found")

// ─── Spaces ──────────────────────────────────────────────────────────────────

func (s *Store) ListSpaces(ctx context.Context) ([]model.Space, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, title, ord FROM spaces ORDER BY ord, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Space
	for rows.Next() {
		var sp model.Space
		if err := rows.Scan(&sp.Key, &sp.Title, &sp.Order); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *Store) GetSpace(ctx context.Context, key string) (*model.Space, error) {
	var sp model.Space
	err := s.pool.QueryRow(ctx, `SELECT key, title, ord FROM spaces WHERE key=$1`, key).
		Scan(&sp.Key, &sp.Title, &sp.Order)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

func (s *Store) UpsertSpace(ctx context.Context, sp model.Space) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO spaces (key, title, ord) VALUES ($1,$2,$3)
		ON CONFLICT (key) DO UPDATE SET title=EXCLUDED.title, ord=EXCLUDED.ord`,
		sp.Key, sp.Title, sp.Order)
	return err
}

func (s *Store) DeleteSpace(ctx context.Context, key string) error {
	return s.execDelete(ctx, `DELETE FROM spaces WHERE key=$1`, key)
}

// ─── Apps ────────────────────────────────────────────────────────────────────

func (s *Store) ListApps(ctx context.Context) ([]model.App, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, title, description, base_url, kind, health_url, icon, enabled
		FROM apps ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetApp(ctx context.Context, key string) (*model.App, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT key, title, description, base_url, kind, health_url, icon, enabled
		FROM apps WHERE key=$1`, key)
	a, err := scanApp(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) UpsertApp(ctx context.Context, a model.App) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO apps (key, title, description, base_url, kind, health_url, icon, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (key) DO UPDATE SET
			title=EXCLUDED.title, description=EXCLUDED.description, base_url=EXCLUDED.base_url,
			kind=EXCLUDED.kind, health_url=EXCLUDED.health_url, icon=EXCLUDED.icon, enabled=EXCLUDED.enabled`,
		a.Key, a.Title, a.Description, a.BaseURL, a.Kind, a.HealthURL, a.Icon, a.Enabled)
	return err
}

func (s *Store) DeleteApp(ctx context.Context, key string) error {
	// tiles cascade via the FK.
	return s.execDelete(ctx, `DELETE FROM apps WHERE key=$1`, key)
}

// ─── Tiles ───────────────────────────────────────────────────────────────────

func (s *Store) ListTiles(ctx context.Context) ([]model.Tile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, app_key, space_key, title, description, icon, target, ord,
		       badge, badge_tone, status, external, open_mode, enabled
		FROM tiles ORDER BY ord, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Tile
	for rows.Next() {
		t, err := scanTile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTile(ctx context.Context, key string) (*model.Tile, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT key, app_key, space_key, title, description, icon, target, ord,
		       badge, badge_tone, status, external, open_mode, enabled
		FROM tiles WHERE key=$1`, key)
	t, err := scanTile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) UpsertTile(ctx context.Context, t model.Tile) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tiles (key, app_key, space_key, title, description, icon, target, ord,
		                   badge, badge_tone, status, external, open_mode, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (key) DO UPDATE SET
			app_key=EXCLUDED.app_key, space_key=EXCLUDED.space_key, title=EXCLUDED.title,
			description=EXCLUDED.description, icon=EXCLUDED.icon, target=EXCLUDED.target,
			ord=EXCLUDED.ord, badge=EXCLUDED.badge, badge_tone=EXCLUDED.badge_tone,
			status=EXCLUDED.status, external=EXCLUDED.external, open_mode=EXCLUDED.open_mode,
			enabled=EXCLUDED.enabled`,
		t.Key, t.AppKey, t.SpaceKey, t.Title, t.Description, t.Icon, t.Target, t.Order,
		t.Badge, t.BadgeTone, t.Status, t.External, t.Open, t.Enabled)
	if err != nil && strings.Contains(err.Error(), "violates foreign key") {
		return fmt.Errorf("%w: app_key or space_key does not exist", err)
	}
	return err
}

func (s *Store) DeleteTile(ctx context.Context, key string) error {
	return s.execDelete(ctx, `DELETE FROM tiles WHERE key=$1`, key)
}

// ─── Launchpad assembly ──────────────────────────────────────────────────────

// Launchpad assembles the ordered spaces with their tiles resolved for
// rendering. A tile whose app is disabled, or that is itself disabled, or that
// resolves to no destination, is included but marked Disabled (so "coming soon"
// cards keep showing). Spaces with no tiles are omitted.
func (s *Store) Launchpad(ctx context.Context) (model.Launchpad, error) {
	spaces, err := s.ListSpaces(ctx)
	if err != nil {
		return model.Launchpad{}, err
	}
	apps, err := s.ListApps(ctx)
	if err != nil {
		return model.Launchpad{}, err
	}
	byApp := make(map[string]model.App, len(apps))
	for _, a := range apps {
		byApp[a.Key] = a
	}
	tiles, err := s.ListTiles(ctx)
	if err != nil {
		return model.Launchpad{}, err
	}

	bySpace := make(map[string][]model.LaunchTile)
	for _, t := range tiles {
		app, ok := byApp[t.AppKey]
		if !ok {
			continue // orphan tile (shouldn't happen — FK) — skip
		}
		href := computeHref(app.BaseURL, t.Target, t.External)
		// Resolve the open mode: an explicit choice wins; unset falls back to the
		// legacy external flag (external tools always meant "new tab"), else inline.
		open := t.Open
		if open == "" {
			if t.External {
				open = "newtab"
			} else {
				open = "inline"
			}
		}
		bySpace[t.SpaceKey] = append(bySpace[t.SpaceKey], model.LaunchTile{
			Key:         t.Key,
			Title:       t.Title,
			Description: t.Description,
			Icon:        t.Icon,
			Href:        href,
			Order:       t.Order,
			Badge:       t.Badge,
			BadgeTone:   t.BadgeTone,
			Status:      t.Status,
			External:    t.External,
			Open:        open,
			Disabled:    !t.Enabled || !app.Enabled || href == "",
		})
	}

	lp := model.Launchpad{}
	for _, sp := range spaces {
		ts := bySpace[sp.Key]
		if len(ts) == 0 {
			continue
		}
		lp.Spaces = append(lp.Spaces, model.LaunchSpace{
			Key: sp.Key, Title: sp.Title, Order: sp.Order, Tiles: ts,
		})
	}
	return lp, nil
}

// computeHref joins an app base_url with a tile target. An absolute or
// site-rooted target is used verbatim; otherwise it is appended to base_url.
func computeHref(baseURL, target string, external bool) string {
	target = strings.TrimSpace(target)
	baseURL = strings.TrimSpace(baseURL)
	if external || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if target != "" {
			return target
		}
		return baseURL
	}
	if strings.HasPrefix(target, "/") {
		return target // site-absolute
	}
	if baseURL == "" {
		return target
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	return baseURL + target
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanApp(r rowScanner) (model.App, error) {
	var a model.App
	err := r.Scan(&a.Key, &a.Title, &a.Description, &a.BaseURL, &a.Kind, &a.HealthURL, &a.Icon, &a.Enabled)
	return a, err
}

func scanTile(r rowScanner) (model.Tile, error) {
	var t model.Tile
	err := r.Scan(&t.Key, &t.AppKey, &t.SpaceKey, &t.Title, &t.Description, &t.Icon, &t.Target,
		&t.Order, &t.Badge, &t.BadgeTone, &t.Status, &t.External, &t.Open, &t.Enabled)
	return t, err
}

func (s *Store) execDelete(ctx context.Context, sql, key string) error {
	tag, err := s.pool.Exec(ctx, sql, key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
