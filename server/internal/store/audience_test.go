package store

import (
	"context"
	"slices"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/db"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// tileKeys lists the tiles a launchpad shows, space by space.
func tileKeys(lp model.Launchpad) []string {
	var out []string
	for _, sp := range lp.Spaces {
		for _, t := range sp.Tiles {
			out = append(out, sp.Key+"/"+t.Key)
		}
	}
	return out
}

// A caller sees a tile when its audience is empty or names one of the
// caller's roles — and only in a space it may see. A space left with nothing
// to show is not shown.
func TestAssembleLaunchpadFiltersByAudience(t *testing.T) {
	apps := []model.App{{Key: "chino", Enabled: true, BaseURL: "/chino/"}, {Key: "katalog", Enabled: true, BaseURL: "/katalog/"}}
	spaces := []model.Space{
		{Key: "apps", Order: 10},
		{Key: "manage", Order: 20},
		{Key: "ops", Order: 30, Audience: []string{"ops"}},
	}
	tiles := []model.Tile{
		{Key: "chino.open", AppKey: "chino", SpaceKey: "apps", Enabled: true},
		{Key: "katalog.catalog", AppKey: "katalog", SpaceKey: "manage", Enabled: true, Audience: []string{"zaentrum-admin"}},
		{Key: "katalog.ops", AppKey: "katalog", SpaceKey: "ops", Enabled: true},
		{Key: "chino.beta", AppKey: "chino", SpaceKey: "apps", Enabled: true, Audience: []string{"beta", "zaentrum-admin"}},
	}
	for _, c := range []struct {
		name  string
		roles []string
		want  []string
	}{
		{"a viewer", []string{"zaentrum-user"}, []string{"apps/chino.open"}},
		{"nobody at all", nil, []string{"apps/chino.open"}},
		{"an admin", []string{"zaentrum-user", "zaentrum-admin"}, []string{"apps/chino.open", "apps/chino.beta", "manage/katalog.catalog"}},
		{"a tester", []string{"beta"}, []string{"apps/chino.open", "apps/chino.beta"}},
		{"operations", []string{"ops"}, []string{"apps/chino.open", "ops/katalog.ops"}},
	} {
		if got := tileKeys(AssembleLaunchpad(spaces, apps, tiles, c.roles)); !slices.Equal(got, c.want) {
			t.Errorf("%s sees %v, want %v", c.name, got, c.want)
		}
	}
}

func seededAudience(t *testing.T, st *Store, key string) []string {
	t.Helper()
	tl, err := st.GetTile(context.Background(), key)
	if err != nil {
		t.Fatalf("tile %s: %v", key, err)
	}
	return tl.Audience
}

// A fresh registry: the catalog tiles are the admin role's, the products are
// everyone's — for the admin role portal-api runs with.
func TestSeedNamesAudiences(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, db.Migrations, "acme-admin"); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]string{
		"chino.open": {}, "tv.open": {}, "musig.open": {},
		"katalog.catalog": {"acme-admin"}, "katalog-manage.open": {"acme-admin"},
	} {
		if got := seededAudience(t, st, key); !slices.Equal(got, want) {
			t.Errorf("%s audience = %q, want %q", key, got, want)
		}
	}
	lp, err := st.Launchpad(ctx, []string{"zaentrum-user"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tileKeys(lp); !slices.Equal(got, []string{"apps/chino.open", "apps/tv.open", "apps/musig.open"}) {
		t.Errorf("a viewer's launchpad = %v", got)
	}
	lp, _ = st.Launchpad(ctx, []string{"acme-admin"})
	if got := tileKeys(lp); len(got) != 5 {
		t.Errorf("an admin's launchpad = %v", got)
	}
	// A seed tile an admin deleted comes back on the next boot — with its
	// audience, not with everyone's.
	exec(t, st, `DELETE FROM tiles WHERE key = 'katalog-manage.open'`)
	if err := st.Migrate(ctx, db.Migrations, "acme-admin"); err != nil {
		t.Fatal(err)
	}
	if got := seededAudience(t, st, "katalog-manage.open"); !slices.Equal(got, []string{"acme-admin"}) {
		t.Errorf("a re-seeded admin tile = %q", got)
	}
}

// A registry from before audiences: the boot that brings the column makes
// what were admin tiles admin-only, once. An admin's choice afterwards —
// opening one to everyone — survives every later boot.
func TestAudienceBackfillRunsOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	// The registry as it was: the schema and seed without audiences.
	exec(t, st, `
		CREATE TABLE spaces (key text PRIMARY KEY, title text NOT NULL, ord int NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE apps (key text PRIMARY KEY, title text NOT NULL, description text NOT NULL DEFAULT '', base_url text NOT NULL DEFAULT '',
			kind text NOT NULL DEFAULT 'tool', health_url text NOT NULL DEFAULT '', icon text NOT NULL DEFAULT '', enabled boolean NOT NULL DEFAULT true,
			created_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE tiles (key text PRIMARY KEY, app_key text NOT NULL REFERENCES apps(key) ON DELETE CASCADE,
			space_key text NOT NULL REFERENCES spaces(key) ON DELETE CASCADE, title text NOT NULL, description text NOT NULL DEFAULT '',
			icon text NOT NULL DEFAULT '', target text NOT NULL DEFAULT '', ord int NOT NULL DEFAULT 0, badge text NOT NULL DEFAULT '',
			badge_tone text NOT NULL DEFAULT '', status text NOT NULL DEFAULT '', external boolean NOT NULL DEFAULT false,
			enabled boolean NOT NULL DEFAULT true, created_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO spaces (key, title, ord) VALUES ('apps', 'apps', 10), ('manage', 'manage', 20), ('ops', 'ops', 30);
		INSERT INTO apps (key, title, kind) VALUES ('chino', 'chino', 'product'), ('katalog', 'Catalog', 'manage'),
			('katalog-manage', 'Catalog Management', 'manage'), ('grafana', 'grafana', 'manage'), ('wiki', 'wiki', 'tool');
		INSERT INTO tiles (key, app_key, space_key, title, badge) VALUES
			('chino.open', 'chino', 'apps', 'chino', 'ready'),
			('katalog.catalog', 'katalog', 'manage', 'Catalog', 'admin'),
			('katalog-manage.open', 'katalog-manage', 'manage', 'Catalog Management', 'admin'),
			('grafana.open', 'grafana', 'ops', 'grafana', ''),
			('wiki.admin', 'wiki', 'ops', 'wiki admin', 'admin'),
			('wiki.open', 'wiki', 'apps', 'wiki', '');`)
	if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
		t.Fatal(err)
	}
	admin := []string{"zaentrum-admin"}
	for key, want := range map[string][]string{
		"chino.open": {}, "wiki.open": {},
		"katalog.catalog": admin, "katalog-manage.open": admin, // the catalog
		"grafana.open": admin, // a manage app's tile
		"wiki.admin":   admin, // badged admin
	} {
		if got := seededAudience(t, st, key); !slices.Equal(got, want) {
			t.Errorf("%s audience = %q, want %q", key, got, want)
		}
	}
	// The admin opens the catalog to everyone; the next boot leaves it.
	exec(t, st, `UPDATE tiles SET audience = '{}' WHERE key = 'katalog.catalog'`)
	if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
		t.Fatal(err)
	}
	if got := seededAudience(t, st, "katalog.catalog"); len(got) != 0 {
		t.Errorf("an admin's choice was reverted on boot: %q", got)
	}
}

// A write without an audience keeps the stored one — an addon's refresh, an
// older console — and an empty one is everyone's.
func TestUpsertKeepsAnAudienceItIsNotGiven(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
		t.Fatal(err)
	}
	tl, _ := st.GetTile(ctx, "katalog.catalog")
	tl.Title, tl.Audience = "renamed", nil
	if err := st.UpsertTile(ctx, *tl); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetTile(ctx, "katalog.catalog"); got.Title != "renamed" || !slices.Equal(got.Audience, []string{"zaentrum-admin"}) {
		t.Errorf("tile = %+v", got)
	}
	tl.Audience = []string{}
	_ = st.UpsertTile(ctx, *tl)
	if got, _ := st.GetTile(ctx, "katalog.catalog"); len(got.Audience) != 0 {
		t.Errorf("an empty audience is everyone's: %q", got.Audience)
	}
	if err := st.UpsertSpace(ctx, model.Space{Key: "ops", Title: "ops", Audience: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertSpace(ctx, model.Space{Key: "ops", Title: "operations"})
	if got, _ := st.GetSpace(ctx, "ops"); got.Title != "operations" || !slices.Equal(got.Audience, []string{"ops"}) {
		t.Errorf("space = %+v", got)
	}
	// A new tile without one is everyone's.
	if err := st.UpsertTile(ctx, model.Tile{Key: "chino.second", AppKey: "chino", SpaceKey: "apps", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetTile(ctx, "chino.second"); got.Audience == nil || len(got.Audience) != 0 {
		t.Errorf("a new tile's audience = %#v", got.Audience)
	}
}
