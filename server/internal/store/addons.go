package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ─── Addons ──────────────────────────────────────────────────────────────────

// ErrWorkloadClaimed is an install whose component names a workload another
// addon already declares (the UNIQUE(workload) constraint, reached when two
// installs race past the API's own check).
var ErrWorkloadClaimed = errors.New("a component workload is already declared by another addon")

// ownedTiles matches the tiles the platform created for addon $1: addon.<key>
// and addon.<key>.<anything>. Must agree with api.ownsTile and with the
// backfill in 008_addons.sql. left() instead of LIKE, so a key is never a
// pattern.
const ownedTiles = `(key = 'addon.' || $1 OR left(key, length($1) + 7) = 'addon.' || $1 || '.')`

// AddonInstall is everything installing (or refreshing) an addon writes.
type AddonInstall struct {
	App        model.App
	Space      *model.Space
	Tiles      []model.Tile
	Rows       []model.Extension
	Addon      model.Addon // key, address, version, manifest and its hash
	Components []model.AddonComponent
}

// InstallAddon writes an install in ONE transaction. An install lands whole or
// not at all: a failure half-way must never leave tiles from one manifest
// beside a component list from another.
//
// Refresh replaces, never merges — tiles, slot rows and components the
// manifest no longer declares are deleted. installed_at survives a refresh;
// refreshed_at does not.
func (s *Store) InstallAddon(ctx context.Context, in AddonInstall) error {
	key := in.App.Key
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := upsertApp(ctx, tx, in.App); err != nil {
			return fmt.Errorf("app: %w", err)
		}
		if in.Space != nil {
			if err := upsertSpace(ctx, tx, *in.Space); err != nil {
				return fmt.Errorf("space: %w", err)
			}
		}
		keep := make([]string, 0, len(in.Tiles))
		for _, t := range in.Tiles {
			keep = append(keep, t.Key)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tiles WHERE `+ownedTiles+` AND NOT (key = ANY($2))`, key, keep); err != nil {
			return fmt.Errorf("tiles: %w", err)
		}
		for _, t := range in.Tiles {
			if err := upsertTile(ctx, tx, t); err != nil {
				return fmt.Errorf("tile %s: %w", t.Key, err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM ui_extensions WHERE addon = $1`, key); err != nil {
			return fmt.Errorf("rows: %w", err)
		}
		for _, row := range in.Rows {
			if err := upsertExtension(ctx, tx, row); err != nil {
				return fmt.Errorf("row %s: %w", row.Key, err)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO addons (key, address, version, manifest, manifest_sha256, chart_ref, chart_version, installed_at, refreshed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
			ON CONFLICT (key) DO UPDATE SET
				address = EXCLUDED.address, version = EXCLUDED.version, manifest = EXCLUDED.manifest,
				manifest_sha256 = EXCLUDED.manifest_sha256, chart_ref = EXCLUDED.chart_ref,
				chart_version = EXCLUDED.chart_version, refreshed_at = now()`,
			key, in.Addon.Address, in.Addon.Version, manifestParam(in.Addon.Manifest), in.Addon.ManifestSHA256,
			in.Addon.ChartRef, in.Addon.ChartVersion); err != nil {
			return fmt.Errorf("addon: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM addon_components WHERE addon_key = $1`, key); err != nil {
			return fmt.Errorf("components: %w", err)
		}
		for _, c := range in.Components {
			if _, err := tx.Exec(ctx, `
				INSERT INTO addon_components (addon_key, name, workload, role, summary, ord)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				key, c.Name, c.Workload, c.Role, c.Summary, c.Order); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return fmt.Errorf("component %s (workload %s): %w", c.Name, c.Workload, ErrWorkloadClaimed)
				}
				return fmt.Errorf("component %s: %w", c.Name, err)
			}
		}
		return nil
	})
}

// manifestParam hands jsonb a NULL for an absent manifest rather than an
// empty (invalid) document.
func manifestParam(m []byte) any {
	if len(m) == 0 {
		return nil
	}
	return string(m)
}

// AddonRemoval is what removing an addon deleted, and the workloads it
// declared — which the platform does not delete.
type AddonRemoval struct {
	Tiles     int
	Rows      int
	Spaces    []string
	Workloads []string
}

// RemoveAddon deletes everything the platform created for an addon, in one
// transaction. declaredSpace is the space the addon's manifest brought; it is
// removed once nothing else lives there. Empty means the addon predates the
// addons table, and any space its tiles leave empty goes with it.
//
// The addons and addon_components rows go with the app (ON DELETE CASCADE).
func (s *Store) RemoveAddon(ctx context.Context, key, declaredSpace string) (AddonRemoval, error) {
	var out AddonRemoval
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT workload FROM addon_components WHERE addon_key = $1 ORDER BY ord, name`, key)
		if err != nil {
			return err
		}
		if out.Workloads, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM ui_extensions WHERE addon = $1`, key)
		if err != nil {
			return fmt.Errorf("rows: %w", err)
		}
		out.Rows = int(tag.RowsAffected())
		rows, err = tx.Query(ctx, `DELETE FROM tiles WHERE `+ownedTiles+` RETURNING space_key`, key)
		if err != nil {
			return fmt.Errorf("tiles: %w", err)
		}
		emptied, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("tiles: %w", err)
		}
		out.Tiles = len(emptied)

		candidates := emptied
		if declaredSpace != "" {
			candidates = []string{declaredSpace}
		}
		seen := map[string]bool{}
		for _, sp := range candidates {
			if seen[sp] {
				continue
			}
			seen[sp] = true
			// An admin may have moved their own tiles in; then it stays.
			tag, err := tx.Exec(ctx, `
				DELETE FROM spaces WHERE key = $1
				AND NOT EXISTS (SELECT 1 FROM tiles WHERE space_key = $1)`, sp)
			if err != nil {
				return fmt.Errorf("space %s: %w", sp, err)
			}
			if tag.RowsAffected() > 0 {
				out.Spaces = append(out.Spaces, sp)
			}
		}
		tag, err = tx.Exec(ctx, `DELETE FROM apps WHERE key = $1`, key)
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
		if tag.RowsAffected() == 0 && out.Rows == 0 && out.Tiles == 0 {
			return ErrNotFound
		}
		return nil
	})
	return out, err
}

const addonCols = `
	ad.key, ad.address, ad.version, ad.manifest, ad.manifest_sha256, ad.installed_at, ad.refreshed_at,
	ad.chart_ref, ad.chart_version, a.title,
	(SELECT count(*) FROM tiles WHERE ` + ownedTilesOf + `),
	(SELECT count(*) FROM ui_extensions e WHERE e.addon = ad.key)`

// ownedTilesOf is ownedTiles against the addon row in a join instead of a
// parameter.
const ownedTilesOf = `(key = 'addon.' || ad.key OR left(key, length(ad.key) + 7) = 'addon.' || ad.key || '.')`

func scanAddon(r rowScanner) (model.Addon, error) {
	var (
		ad             model.Addon
		manifest       []byte
		tiles, extRows int64
	)
	err := r.Scan(&ad.Key, &ad.Address, &ad.Version, &manifest, &ad.ManifestSHA256,
		&ad.InstalledAt, &ad.RefreshedAt, &ad.ChartRef, &ad.ChartVersion, &ad.Title, &tiles, &extRows)
	ad.Manifest, ad.Tiles, ad.Rows = manifest, int(tiles), int(extRows)
	return ad, err
}

// ListAddons returns every installed addon with its components, oldest first.
func (s *Store) ListAddons(ctx context.Context) ([]model.Addon, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+addonCols+`
		FROM addons ad JOIN apps a ON a.key = ad.key
		ORDER BY ad.installed_at, ad.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Addon
	for rows.Next() {
		ad, err := scanAddon(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ad)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	components, err := s.addonComponents(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Components = components[out[i].Key]
	}
	return out, nil
}

// GetAddon returns one installed addon with its components.
func (s *Store) GetAddon(ctx context.Context, key string) (*model.Addon, error) {
	ad, err := scanAddon(s.pool.QueryRow(ctx, `SELECT `+addonCols+`
		FROM addons ad JOIN apps a ON a.key = ad.key
		WHERE ad.key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	components, err := s.addonComponents(ctx, key)
	if err != nil {
		return nil, err
	}
	ad.Components = components[key]
	return &ad, nil
}

// addonComponents loads components by addon key; key "" loads all.
func (s *Store) addonComponents(ctx context.Context, key string) (map[string][]model.AddonComponent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT addon_key, name, workload, role, summary, ord FROM addon_components
		WHERE $1 = '' OR addon_key = $1
		ORDER BY addon_key, ord, name`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.AddonComponent{}
	for rows.Next() {
		var (
			addon string
			c     model.AddonComponent
		)
		if err := rows.Scan(&addon, &c.Name, &c.Workload, &c.Role, &c.Summary, &c.Order); err != nil {
			return nil, err
		}
		out[addon] = append(out[addon], c)
	}
	return out, rows.Err()
}

// WorkloadClaims maps every declared workload to the addon that declares it.
func (s *Store) WorkloadClaims(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT workload, addon_key FROM addon_components`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var workload, addon string
		if err := rows.Scan(&workload, &addon); err != nil {
			return nil, err
		}
		out[workload] = addon
	}
	return out, rows.Err()
}

// SetRegistrationError records why a chart addon could not be registered; an
// empty message clears the record.
func (s *Store) SetRegistrationError(ctx context.Context, name, msg string) error {
	if msg == "" {
		_, err := s.pool.Exec(ctx, `DELETE FROM addon_registration_errors WHERE name = $1`, name)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO addon_registration_errors (name, error, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (name) DO UPDATE SET error = EXCLUDED.error, updated_at = now()`, name, msg)
	return err
}

// RegistrationErrors maps each chart addon that could not be registered to why.
func (s *Store) RegistrationErrors(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, error FROM addon_registration_errors`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, msg string
		if err := rows.Scan(&name, &msg); err != nil {
			return nil, err
		}
		out[name] = msg
	}
	return out, rows.Err()
}
