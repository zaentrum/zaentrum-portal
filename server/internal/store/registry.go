package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ErrNotFound is returned when a keyed row does not exist.
var ErrNotFound = errors.New("not found")

// ErrCore refuses to delete a core entry: an app or space the platform stands
// on (migration 011).
var ErrCore = errors.New("a core entry of the platform")

// execer is what both the pool and a transaction offer, so an upsert is the
// same statement whether it runs alone or inside an addon install.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ─── Spaces ──────────────────────────────────────────────────────────────────

const spaceCols = `key, title, ord, audience, core`

func scanSpace(r rowScanner) (model.Space, error) {
	var sp model.Space
	err := r.Scan(&sp.Key, &sp.Title, &sp.Order, &sp.Audience, &sp.Core)
	sp.Audience = nonNilRoles(sp.Audience)
	return sp, err
}

func (s *Store) ListSpaces(ctx context.Context) ([]model.Space, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+spaceCols+` FROM spaces ORDER BY ord, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Space
	for rows.Next() {
		sp, err := scanSpace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *Store) GetSpace(ctx context.Context, key string) (*model.Space, error) {
	sp, err := scanSpace(s.pool.QueryRow(ctx, `SELECT `+spaceCols+` FROM spaces WHERE key=$1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

// UpsertSpace writes a space. A nil audience keeps the stored one (a new
// space is for everyone); an empty one is everyone signed in.
func (s *Store) UpsertSpace(ctx context.Context, sp model.Space) error {
	return upsertSpace(ctx, s.pool, sp)
}

func upsertSpace(ctx context.Context, ex execer, sp model.Space) error {
	_, err := ex.Exec(ctx, `
		INSERT INTO spaces (key, title, ord, audience) VALUES ($1,$2,$3, COALESCE($4::text[], '{}'))
		ON CONFLICT (key) DO UPDATE SET title=EXCLUDED.title, ord=EXCLUDED.ord,
			audience = CASE WHEN $4::text[] IS NULL THEN spaces.audience ELSE EXCLUDED.audience END`,
		sp.Key, sp.Title, sp.Order, sp.Audience)
	return err
}

// DeleteSpace deletes a space and, by the foreign key, its tiles — never a
// core space (ErrCore).
func (s *Store) DeleteSpace(ctx context.Context, key string) error {
	return s.deleteUnlessCore(ctx, "spaces", key)
}

// ─── Apps ────────────────────────────────────────────────────────────────────

func (s *Store) ListApps(ctx context.Context) ([]model.App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+appCols+` FROM apps ORDER BY key`)
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
	row := s.pool.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE key=$1`, key)
	a, err := scanApp(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

const appCols = `key, title, description, base_url, kind, health_url, icon, enabled, proxy_url, core`

// UpsertApp writes an app. Core is the platform's: it is never written here.
func (s *Store) UpsertApp(ctx context.Context, a model.App) error {
	return upsertApp(ctx, s.pool, a)
}

func upsertApp(ctx context.Context, ex execer, a model.App) error {
	_, err := ex.Exec(ctx, `
		INSERT INTO apps (key, title, description, base_url, kind, health_url, icon, enabled, proxy_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (key) DO UPDATE SET
			title=EXCLUDED.title, description=EXCLUDED.description, base_url=EXCLUDED.base_url,
			kind=EXCLUDED.kind, health_url=EXCLUDED.health_url, icon=EXCLUDED.icon,
			enabled=EXCLUDED.enabled, proxy_url=EXCLUDED.proxy_url`,
		a.Key, a.Title, a.Description, a.BaseURL, a.Kind, a.HealthURL, a.Icon, a.Enabled, a.ProxyURL)
	return err
}

// DeleteApp deletes an app and, by the foreign key, its tiles — never a core
// app (ErrCore).
func (s *Store) DeleteApp(ctx context.Context, key string) error {
	return s.deleteUnlessCore(ctx, "apps", key)
}

// deleteUnlessCore deletes a keyed row of apps or spaces unless it is core:
// ErrCore when it is, ErrNotFound when there is none.
func (s *Store) deleteUnlessCore(ctx context.Context, table, key string) error {
	if table != "apps" && table != "spaces" {
		return fmt.Errorf("no core rows in %s", table)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM `+table+` WHERE key=$1 AND NOT core`, key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var core bool
	switch err := s.pool.QueryRow(ctx, `SELECT core FROM `+table+` WHERE key=$1`, key).Scan(&core); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case core:
		return ErrCore
	}
	return ErrNotFound // deleted meanwhile
}

// ─── Tiles ───────────────────────────────────────────────────────────────────

const tileCols = `key, app_key, space_key, title, description, icon, target, ord,
		       badge, badge_tone, status, external, open_mode, enabled, audience`

func (s *Store) ListTiles(ctx context.Context) ([]model.Tile, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tileCols+` FROM tiles ORDER BY ord, key`)
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
	row := s.pool.QueryRow(ctx, `SELECT `+tileCols+` FROM tiles WHERE key=$1`, key)
	t, err := scanTile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UpsertTile writes a tile. A nil audience keeps the stored one (a new tile is
// for everyone) — an addon's refresh keeps the audience an admin gave its
// tiles; an empty one is everyone signed in.
func (s *Store) UpsertTile(ctx context.Context, t model.Tile) error {
	return upsertTile(ctx, s.pool, t)
}

func upsertTile(ctx context.Context, ex execer, t model.Tile) error {
	_, err := ex.Exec(ctx, `
		INSERT INTO tiles (key, app_key, space_key, title, description, icon, target, ord,
		                   badge, badge_tone, status, external, open_mode, enabled, audience)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14, COALESCE($15::text[], '{}'))
		ON CONFLICT (key) DO UPDATE SET
			app_key=EXCLUDED.app_key, space_key=EXCLUDED.space_key, title=EXCLUDED.title,
			description=EXCLUDED.description, icon=EXCLUDED.icon, target=EXCLUDED.target,
			ord=EXCLUDED.ord, badge=EXCLUDED.badge, badge_tone=EXCLUDED.badge_tone,
			status=EXCLUDED.status, external=EXCLUDED.external, open_mode=EXCLUDED.open_mode,
			enabled=EXCLUDED.enabled,
			audience = CASE WHEN $15::text[] IS NULL THEN tiles.audience ELSE EXCLUDED.audience END`,
		t.Key, t.AppKey, t.SpaceKey, t.Title, t.Description, t.Icon, t.Target, t.Order,
		t.Badge, t.BadgeTone, t.Status, t.External, t.Open, t.Enabled, t.Audience)
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
// rendering, as a caller holding roles sees them (AssembleLaunchpad).
func (s *Store) Launchpad(ctx context.Context, roles []string) (model.Launchpad, error) {
	spaces, err := s.ListSpaces(ctx)
	if err != nil {
		return model.Launchpad{}, err
	}
	apps, err := s.ListApps(ctx)
	if err != nil {
		return model.Launchpad{}, err
	}
	tiles, err := s.ListTiles(ctx)
	if err != nil {
		return model.Launchpad{}, err
	}
	return AssembleLaunchpad(spaces, apps, tiles, roles), nil
}

// AssembleLaunchpad resolves the registry rows into the launchpad a caller
// holding roles sees: the ordered spaces with their tiles, each only when its
// audience lets the caller see it (model.Visible). A tile whose app is
// disabled, or that is itself disabled, or that resolves to no destination,
// is included but marked Disabled (so "coming soon" cards keep showing).
// Spaces with no tiles to show are omitted. Pure, so the rule is tested
// without a database.
func AssembleLaunchpad(spaces []model.Space, apps []model.App, tiles []model.Tile, roles []string) model.Launchpad {
	byApp := make(map[string]model.App, len(apps))
	for _, a := range apps {
		byApp[a.Key] = a
	}

	bySpace := make(map[string][]model.LaunchTile)
	for _, t := range tiles {
		app, ok := byApp[t.AppKey]
		if !ok {
			continue // orphan tile (shouldn't happen — FK) — skip
		}
		if !model.Visible(t.Audience, roles) {
			continue
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
		if len(ts) == 0 || !model.Visible(sp.Audience, roles) {
			continue
		}
		lp.Spaces = append(lp.Spaces, model.LaunchSpace{
			Key: sp.Key, Title: sp.Title, Order: sp.Order, Tiles: ts,
		})
	}
	return lp
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
	err := r.Scan(&a.Key, &a.Title, &a.Description, &a.BaseURL, &a.Kind, &a.HealthURL,
		&a.Icon, &a.Enabled, &a.ProxyURL, &a.Core)
	return a, err
}

func scanTile(r rowScanner) (model.Tile, error) {
	var t model.Tile
	err := r.Scan(&t.Key, &t.AppKey, &t.SpaceKey, &t.Title, &t.Description, &t.Icon, &t.Target,
		&t.Order, &t.Badge, &t.BadgeTone, &t.Status, &t.External, &t.Open, &t.Enabled, &t.Audience)
	t.Audience = nonNilRoles(t.Audience)
	return t, err
}

// nonNilRoles reads an empty audience as [], which is how it is written out.
func nonNilRoles(roles []string) []string {
	if roles == nil {
		return []string{}
	}
	return roles
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

// ─── UI extensions ─────────────────────────────────────────────────────────

const extCols = `key, addon, slot, kind, label, icon, url, method, status_url, ord, enabled`

func scanExtension(r rowScanner) (model.Extension, error) {
	var e model.Extension
	err := r.Scan(&e.Key, &e.Addon, &e.Slot, &e.Kind, &e.Label, &e.Icon, &e.URL,
		&e.Method, &e.StatusURL, &e.Order, &e.Enabled)
	return e, err
}

// ListExtensions returns every contribution (admin console view).
func (s *Store) ListExtensions(ctx context.Context) ([]model.Extension, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+extCols+` FROM ui_extensions ORDER BY slot, ord, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Extension
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListExtensionsForSlot returns the ENABLED contributions for one slot (the
// product-app read path).
func (s *Store) ListExtensionsForSlot(ctx context.Context, slot string) ([]model.Extension, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+extCols+` FROM ui_extensions WHERE slot=$1 AND enabled ORDER BY ord, key`, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Extension
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetExtension returns one contribution, enabled or not.
func (s *Store) GetExtension(ctx context.Context, key string) (*model.Extension, error) {
	e, err := scanExtension(s.pool.QueryRow(ctx, `SELECT `+extCols+` FROM ui_extensions WHERE key=$1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) UpsertExtension(ctx context.Context, e model.Extension) error {
	return upsertExtension(ctx, s.pool, e)
}

func upsertExtension(ctx context.Context, ex execer, e model.Extension) error {
	_, err := ex.Exec(ctx, `
		INSERT INTO ui_extensions (key, addon, slot, kind, label, icon, url, method, status_url, ord, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (key) DO UPDATE SET
			addon=EXCLUDED.addon, slot=EXCLUDED.slot, kind=EXCLUDED.kind, label=EXCLUDED.label,
			icon=EXCLUDED.icon, url=EXCLUDED.url, method=EXCLUDED.method, status_url=EXCLUDED.status_url,
			ord=EXCLUDED.ord, enabled=EXCLUDED.enabled`,
		e.Key, e.Addon, e.Slot, e.Kind, e.Label, e.Icon, e.URL, e.Method, e.StatusURL, e.Order, e.Enabled)
	return err
}

func (s *Store) DeleteExtension(ctx context.Context, key string) error {
	return s.execDelete(ctx, `DELETE FROM ui_extensions WHERE key=$1`, key)
}

// DeleteExtensionsByAddon removes every contribution an addon owns. Ownership
// is the `addon` column — the reason it exists is exactly this call.
func (s *Store) DeleteExtensionsByAddon(ctx context.Context, addon string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM ui_extensions WHERE addon=$1`, addon)
	return err
}
