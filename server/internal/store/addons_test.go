package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zaentrum/zaentrum-portal/server/db"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// These tests run the real migrations and statements against Postgres. They
// need a DISPOSABLE database — they drop the registry tables first — named by
// PORTAL_TEST_DATABASE_URL, and skip without one.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PORTAL_TEST_DATABASE_URL not set — skipping Postgres tests")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn, "", "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(ctx, `DROP TABLE IF EXISTS addon_components, addons, ui_extensions, tiles, spaces, apps CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	return st
}

// migrationsBefore is the embedded migration set without the files from name
// on — the schema an instance had before that migration shipped.
func migrationsBefore(t *testing.T, name string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(db.Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= name {
			continue
		}
		body, err := fs.ReadFile(db.Migrations, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out["migrations/"+e.Name()] = &fstest.MapFile{Data: body}
	}
	return out
}

func exec(t *testing.T, st *Store, sql string, args ...any) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// The backfill must find every addon installed the old way — by owned tiles or
// by owned slot rows — give it its implicit primary, leave everything else
// alone, and do nothing more on the next boot.
func TestAddonsBackfill(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, migrationsBefore(t, "008")); err != nil {
		t.Fatalf("pre-008 migrations: %v", err)
	}
	exec(t, st, `INSERT INTO apps (key, title, proxy_url) VALUES
		('example', 'example', 'http://example.zaentrum.svc.cluster.local:8080'),
		('tiled',   'tiled',   'http://tiled'),
		('rowsonly','rowsonly',''),
		('plain',   'plain',   'http://plain'),
		('lookalike','lookalike','http://lookalike')`)
	exec(t, st, `INSERT INTO tiles (key, app_key, space_key, title) VALUES
		('addon.example', 'example', 'apps', 'example'),
		('addon.tiled.items', 'tiled', 'apps', 'items'),
		('addon.lookalike2', 'lookalike', 'apps', 'not owned: the dot matters'),
		('plain.open', 'plain', 'apps', 'a hand-made tile')`)
	exec(t, st, `INSERT INTO ui_extensions (key, addon, slot, label) VALUES ('rowsonly.hint', 'rowsonly', 'search.empty', 'hint')`)

	for i := 0; i < 2; i++ { // every boot re-applies every migration
		if err := st.Migrate(ctx, db.Migrations); err != nil {
			t.Fatalf("migrate (boot %d): %v", i+1, err)
		}
	}

	addons, err := st.ListAddons(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.Addon{}
	var keys []string
	for _, ad := range addons {
		got[ad.Key] = ad
		keys = append(keys, ad.Key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "example,rowsonly,tiled" {
		t.Fatalf("backfilled addons = %v, want example,rowsonly,tiled", keys)
	}
	cases := []struct {
		key, address, workload string
		tiles, rows            int
	}{
		{"example", "http://example.zaentrum.svc.cluster.local:8080", "example", 1, 0},
		{"tiled", "http://tiled", "tiled", 1, 0},
		{"rowsonly", "", "", 0, 1}, // no address, so no workload to point at
	}
	for _, c := range cases {
		ad := got[c.key]
		if ad.Address != c.address || ad.Tiles != c.tiles || ad.Rows != c.rows || ad.Manifest != nil {
			t.Errorf("%s = %+v", c.key, ad)
		}
		if c.workload == "" {
			if len(ad.Components) != 0 {
				t.Errorf("%s: components = %+v, want none", c.key, ad.Components)
			}
			continue
		}
		want := model.AddonComponent{Name: c.key, Workload: c.workload, Role: "primary"}
		if len(ad.Components) != 1 || ad.Components[0] != want {
			t.Errorf("%s: components = %+v, want [%+v]", c.key, ad.Components, want)
		}
	}
}

func TestInstallRefreshAndRemoveAddon(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, db.Migrations); err != nil {
		t.Fatal(err)
	}
	app := model.App{Key: "example", Title: "Example", Kind: "tool", Enabled: true, BaseURL: "/portal/app/example", ProxyURL: "http://example"}
	space := &model.Space{Key: "example", Title: "example", Order: 30}
	tile := func(k string) model.Tile {
		return model.Tile{Key: k, AppKey: "example", SpaceKey: "example", Title: k, Open: "inline", Enabled: true}
	}
	in := AddonInstall{
		App: app, Space: space,
		Tiles: []model.Tile{tile("addon.example.items"), tile("addon.example.queue")},
		Rows:  []model.Extension{{Key: "example.hint", Addon: "example", Slot: "search.empty", Kind: "link", Label: "hint", Method: "POST", Enabled: true}},
		Addon: model.Addon{Key: "example", Address: "http://example", Version: "1.0.0", Manifest: []byte(`{"service":"example"}`), ManifestSHA256: "aa"},
		Components: []model.AddonComponent{
			{Name: "example", Workload: "example", Role: "primary"},
			{Name: "worker", Workload: "example-worker", Role: "optional", Summary: "processes the queue", Order: 1},
		},
	}
	if err := st.InstallAddon(ctx, in); err != nil {
		t.Fatalf("install: %v", err)
	}
	first, err := st.GetAddon(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != "1.0.0" || first.Tiles != 2 || first.Rows != 1 || len(first.Components) != 2 || string(first.Manifest) != `{"service": "example"}` {
		t.Errorf("installed = %+v (manifest %s)", first, first.Manifest)
	}
	claims, err := st.WorkloadClaims(ctx)
	if err != nil || claims["example-worker"] != "example" {
		t.Errorf("claims = %v, %v", claims, err)
	}

	// Refresh replaces: a dropped tile and a dropped component disappear, and
	// installed_at survives.
	in.Tiles = in.Tiles[:1]
	in.Components = in.Components[:1]
	in.Addon.Version, in.Addon.ManifestSHA256 = "1.1.0", "bb"
	if err := st.InstallAddon(ctx, in); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	second, err := st.GetAddon(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != "1.1.0" || second.Tiles != 1 || len(second.Components) != 1 || !second.InstalledAt.Equal(first.InstalledAt) {
		t.Errorf("refreshed = %+v", second)
	}

	// A second addon may not take the first one's workload, and the failed
	// install leaves nothing behind.
	other := AddonInstall{
		App:        model.App{Key: "other", Title: "other", Kind: "tool", Enabled: true},
		Addon:      model.Addon{Key: "other"},
		Components: []model.AddonComponent{{Name: "other", Workload: "example", Role: "primary"}},
	}
	if err := st.InstallAddon(ctx, other); !errors.Is(err, ErrWorkloadClaimed) {
		t.Fatalf("claimed workload: err = %v, want ErrWorkloadClaimed", err)
	}
	if _, err := st.GetApp(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a failed install must roll back its app, got %v", err)
	}

	// An admin's own tile in the addon's space keeps the space alive.
	exec(t, st, `INSERT INTO apps (key, title) VALUES ('mine', 'mine')`)
	exec(t, st, `INSERT INTO tiles (key, app_key, space_key, title) VALUES ('mine.open', 'mine', 'example', 'mine')`)
	removed, err := st.RemoveAddon(ctx, "example", "example")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if removed.Tiles != 1 || removed.Rows != 1 || len(removed.Spaces) != 0 || strings.Join(removed.Workloads, ",") != "example" {
		t.Errorf("removed = %+v", removed)
	}
	if _, err := st.GetAddon(ctx, "example"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the addon row must cascade with its app, got %v", err)
	}
	if claims, _ := st.WorkloadClaims(ctx); len(claims) != 0 {
		t.Errorf("components must cascade with the addon: %v", claims)
	}

	// Once the space is empty, removing the addon that brought it removes it.
	exec(t, st, `DELETE FROM tiles WHERE key = 'mine.open'`)
	if err := st.InstallAddon(ctx, in); err != nil {
		t.Fatal(err)
	}
	removed, err = st.RemoveAddon(ctx, "example", "example")
	if err != nil || strings.Join(removed.Spaces, ",") != "example" {
		t.Errorf("removed = %+v, %v", removed, err)
	}
	if _, err := st.RemoveAddon(ctx, "example", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing twice = %v, want ErrNotFound", err)
	}
}
