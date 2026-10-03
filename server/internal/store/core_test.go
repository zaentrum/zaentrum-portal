package store

import (
	"context"
	"errors"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/db"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// The seed's core entries are marked by the migration, and the database
// refuses to delete them — the API's own check is not the only guard — while
// everything else deletes as before.
func TestCoreEntries(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
		t.Fatal(err)
	}
	if app, _ := st.GetApp(ctx, "chino"); !app.Core {
		t.Error("chino is core")
	}
	if app, _ := st.GetApp(ctx, "katalog"); app.Core {
		t.Error("katalog is not core")
	}
	for _, key := range []string{"apps", "manage"} {
		if sp, _ := st.GetSpace(ctx, key); !sp.Core {
			t.Errorf("space %s is core", key)
		}
	}
	if err := st.DeleteApp(ctx, "chino"); !errors.Is(err, ErrCore) {
		t.Errorf("DeleteApp(chino) = %v", err)
	}
	if err := st.DeleteSpace(ctx, "apps"); !errors.Is(err, ErrCore) {
		t.Errorf("DeleteSpace(apps) = %v", err)
	}
	if tl, err := st.GetTile(ctx, "chino.open"); err != nil || tl == nil {
		t.Error("the core app's tile went with a refused delete")
	}
	// An update writes no core.
	app, _ := st.GetApp(ctx, "chino")
	app.Core, app.Title = false, "media"
	if err := st.UpsertApp(ctx, *app); err != nil {
		t.Fatal(err)
	}
	if app, _ := st.GetApp(ctx, "chino"); !app.Core || app.Title != "media" {
		t.Errorf("app = %+v", app)
	}
	if err := st.UpsertApp(ctx, model.App{Key: "mine", Title: "mine", Core: true}); err != nil {
		t.Fatal(err)
	}
	if app, _ := st.GetApp(ctx, "mine"); app.Core {
		t.Error("a write made an app core")
	}
	if err := st.DeleteApp(ctx, "mine"); err != nil {
		t.Errorf("DeleteApp(mine) = %v", err)
	}
	if err := st.DeleteApp(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteApp(none) = %v", err)
	}
	if err := st.DeleteSpace(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteSpace(none) = %v", err)
	}
}

// An addon cannot reach a core space through its manifest: installing one
// that declares "apps" as its own space leaves its title alone, and removing
// it leaves the space.
func TestAddonsLeaveCoreSpacesAlone(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
		t.Fatal(err)
	}
	exec(t, st, `DELETE FROM tiles WHERE space_key = 'apps'`) // a space its addon tile would leave empty
	in := AddonInstall{
		App:   model.App{Key: "squat", Title: "squat", Enabled: true},
		Space: &model.Space{Key: "apps", Title: "squatted", Order: 1},
		Tiles: []model.Tile{{Key: "addon.squat", AppKey: "squat", SpaceKey: "apps", Title: "squat", Enabled: true}},
		Addon: model.Addon{Key: "squat"},
	}
	if err := st.InstallAddon(ctx, in); err != nil {
		t.Fatal(err)
	}
	if sp, _ := st.GetSpace(ctx, "apps"); sp.Title != "apps" || sp.Order != 10 {
		t.Errorf("a manifest retitled a core space: %+v", sp)
	}
	if _, err := st.RemoveAddon(ctx, "squat", "apps"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSpace(ctx, "apps"); err != nil {
		t.Errorf("removing an addon took a core space: %v", err)
	}
}
