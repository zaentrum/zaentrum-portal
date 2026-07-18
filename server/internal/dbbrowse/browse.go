// Package dbbrowse is a curated, read-only database browser for the admin debug
// console. It exposes a fixed whitelist of tables/views (no arbitrary SQL) across
// the platform's per-app databases, each query run inside a read-only transaction
// with a statement timeout, values secret-masked and length-capped. It answers
// the demo operator's "just let me see the tables" without becoming a SQL shell.
package dbbrowse

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/zaentrum-portal/server/internal/redact"
)

const (
	defaultLimit = 100
	maxLimit     = 500
	cellCap      = 400 // max chars kept per cell (long text is truncated)
	queryTimeout = 6 * time.Second
)

// Table is one curated, read-only view into a database.
type Table struct {
	Key         string `json:"key"`
	DB          string `json:"db"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Rows        int    `json:"rows"` // live count, -1 if unavailable (filled by Tables)
	query       string // SELECT with explicit columns + ORDER BY (LIMIT/OFFSET appended)
	count       string // SELECT count(*)
}

// Page is a slice of one table's rows, columns first, values stringified.
type Page struct {
	Table   string     `json:"table"`
	DB      string     `json:"db"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	Total   int        `json:"total"`
	Limit   int        `json:"limit"`
	Offset  int        `json:"offset"`
}

// curated is the whitelist. Queries are constant (no interpolation of user input;
// only int LIMIT/OFFSET are appended), columns are explicit to avoid blob columns,
// and ORDER BY keeps paging stable. Extend here, never from the request.
var curated = []Table{
	{
		Key: "katalog.items", DB: "katalog",
		Label: "catalog · items", Description: "every catalog item (movies, series, episodes)",
		query: `SELECT id, type, title, sorttitle, year, parent_id, seasonnumber, episodenumber, durationms, modifiedat
		        FROM com_nalet_katalog_items
		        ORDER BY sorttitle NULLS LAST, seasonnumber NULLS FIRST, episodenumber NULLS FIRST`,
		count: `SELECT count(*) FROM com_nalet_katalog_items`,
	},
	{
		Key: "katalog.status", DB: "katalog",
		Label: "catalog · pipeline status", Description: "per-item overall pipeline status (view)",
		query: `SELECT item_id, overallstatus, donecount, pendingcount, failedcount, inprogresscount, totalsteps, laststepfinishedat
		        FROM com_nalet_katalog_itemoverallstatus
		        ORDER BY laststepfinishedat DESC NULLS LAST`,
		count: `SELECT count(*) FROM com_nalet_katalog_itemoverallstatus`,
	},
	{
		Key: "katalog.steps", DB: "katalog",
		Label: "catalog · processing steps", Description: "scan / enrich / analyze / transcode / package steps",
		query: `SELECT item_id, step, status, attempts, startedat, finishedat, error
		        FROM com_nalet_katalog_itemprocessingsteps
		        ORDER BY COALESCE(finishedat, startedat) DESC NULLS LAST`,
		count: `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps`,
	},
	{
		Key: "katalog.playback", DB: "katalog",
		Label: "catalog · playback assets", Description: "packaged/source playback assets per item",
		query: `SELECT item_id, kind, isprimary, codec, resolution, sizebytes, durationms, path
		        FROM com_nalet_katalog_playbackassets
		        ORDER BY item_id`,
		count: `SELECT count(*) FROM com_nalet_katalog_playbackassets`,
	},
	{
		Key: "portal.apps", DB: "portal",
		Label: "portal · apps", Description: "launchpad app registry",
		query: `SELECT * FROM apps ORDER BY 1`,
		count: `SELECT count(*) FROM apps`,
	},
	{
		Key: "portal.spaces", DB: "portal",
		Label: "portal · spaces", Description: "launchpad space registry",
		query: `SELECT * FROM spaces ORDER BY 1`,
		count: `SELECT count(*) FROM spaces`,
	},
	{
		Key: "portal.tiles", DB: "portal",
		Label: "portal · tiles", Description: "launchpad tile registry",
		query: `SELECT * FROM tiles ORDER BY 1`,
		count: `SELECT count(*) FROM tiles`,
	},
}

// Browser opens (and caches) a read-only pool per database, all derived from the
// portal-api's base DSN with the database name swapped.
type Browser struct {
	baseDSN string
	user    string
	pass    string
	mu      sync.Mutex
	pools   map[string]*pgxpool.Pool
}

// New constructs a browser from the portal-api's datasource config. The DSN must
// be non-empty (the portal always has one).
func New(dsn, user, pass string) *Browser {
	return &Browser{baseDSN: dsn, user: user, pass: pass, pools: map[string]*pgxpool.Pool{}}
}

// Available reports whether a base DSN is configured.
func (b *Browser) Available() bool { return b != nil && b.baseDSN != "" }

// Tables returns the curated whitelist, each with a live row count (best-effort).
func (b *Browser) Tables(ctx context.Context) []Table {
	out := make([]Table, 0, len(curated))
	for _, t := range curated {
		tt := t
		tt.query, tt.count = "", "" // never leak the SQL to the client
		if pool, err := b.pool(ctx, t.DB); err == nil {
			cctx, cancel := context.WithTimeout(ctx, queryTimeout)
			var n int
			if err := pool.QueryRow(cctx, t.count).Scan(&n); err == nil {
				tt.Rows = n
			} else {
				tt.Rows = -1
			}
			cancel()
		} else {
			tt.Rows = -1
		}
		out = append(out, tt)
	}
	return out
}

// Rows returns a page of a curated table, read-only, secret-masked, length-capped.
func (b *Browser) Rows(ctx context.Context, key string, limit, offset int) (*Page, error) {
	tbl := find(key)
	if tbl == nil {
		return nil, fmt.Errorf("unknown table %q", key)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	pool, err := b.pool(ctx, tbl.DB)
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	// One read-only transaction guarantees the browser can never mutate, no matter
	// what a curated query says.
	tx, err := pool.BeginTx(cctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(cctx)

	page := &Page{Table: tbl.Key, DB: tbl.DB, Limit: limit, Offset: offset}
	_ = tx.QueryRow(cctx, tbl.count).Scan(&page.Total) // best-effort

	q := fmt.Sprintf("%s LIMIT %d OFFSET %d", tbl.query, limit, offset)
	rows, err := tx.Query(cctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	page.Columns = make([]string, len(fds))
	secretCol := make([]bool, len(fds))
	for i, fd := range fds {
		page.Columns[i] = string(fd.Name)
		secretCol[i] = redact.LooksSecretKey(string(fd.Name))
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		r := make([]string, len(vals))
		for i, v := range vals {
			r[i] = cell(v, secretCol[i])
		}
		page.Rows = append(page.Rows, r)
	}
	return page, rows.Err()
}

// Close releases every cached pool.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pools {
		p.Close()
	}
	b.pools = map[string]*pgxpool.Pool{}
}

// pool returns (opening + caching on first use) a small read-only pool for db.
func (b *Browser) pool(ctx context.Context, db string) (*pgxpool.Pool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.pools[db]; ok {
		return p, nil
	}
	cfg, err := pgxpool.ParseConfig(b.baseDSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.ConnConfig.Database = db
	if b.user != "" {
		cfg.ConnConfig.User = b.user
	}
	if b.pass != "" {
		cfg.ConnConfig.Password = b.pass
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "6000"
	cfg.MaxConns = 2
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool for %q: %w", db, err)
	}
	b.pools[db] = p
	return p, nil
}

func find(key string) *Table {
	for i := range curated {
		if curated[i].Key == key {
			return &curated[i]
		}
	}
	return nil
}

// cell stringifies a value: nil → "", timestamps → RFC3339, secret columns fully
// masked, everything else redacted for inline credentials and length-capped.
func cell(v any, secret bool) string {
	if v == nil {
		return ""
	}
	var s string
	switch t := v.(type) {
	case time.Time:
		s = t.UTC().Format(time.RFC3339)
	case []byte:
		s = fmt.Sprintf("<%d bytes>", len(t))
	default:
		s = fmt.Sprintf("%v", v)
	}
	if secret {
		return "***REDACTED***"
	}
	s = redact.Secrets(s)
	if len(s) > cellCap {
		s = s[:cellCap] + "…"
	}
	return s
}
