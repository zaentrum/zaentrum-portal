package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// A refresh — the addon added again by its address, naming no space — puts
// its tiles back in the space it was installed into, not into whichever space
// comes first. A space the request names is the new one; one that does not
// exist is refused before anything is written; and once the recorded space is
// gone, the first one takes them.
func TestARefreshKeepsTheSpaceTheAddonWasInstalledInto(t *testing.T) {
	addr := manifestServer(t, localhostManifest)
	fake := newFakeStore()
	fake.spaces["media"] = model.Space{Key: "media", Title: "media", Order: 50} // after apps
	a := &API{addons: fake}

	// install records what the store would: the app and the addon.
	install := func(body map[string]any) (model.Addon, []model.Tile, string) {
		t.Helper()
		rec := postInstall(t, a, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("install %v = %d %s", body, rec.Code, rec.Body)
		}
		var out struct {
			InstallSpace string `json:"installSpace"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		in := fake.installs[len(fake.installs)-1]
		fake.apps[in.App.Key], fake.addons[in.Addon.Key] = in.App, in.Addon
		return in.Addon, in.Tiles, out.InstallSpace
	}
	spaceOf := func(tiles []model.Tile) string {
		t.Helper()
		if len(tiles) != 1 {
			t.Fatalf("tiles = %+v", tiles)
		}
		return tiles[0].SpaceKey
	}

	ad, tiles, answered := install(map[string]any{"proxyUrl": addr, "space": "media"})
	if ad.Space != "media" || spaceOf(tiles) != "media" || answered != "media" {
		t.Fatalf("installed into media: addon space %q, tile in %q, answer %q", ad.Space, spaceOf(tiles), answered)
	}

	ad, tiles, answered = install(map[string]any{"proxyUrl": addr})
	if ad.Space != "media" || spaceOf(tiles) != "media" || answered != "media" {
		t.Errorf("refreshed: addon space %q, tile in %q, answer %q — want media, where it was installed", ad.Space, spaceOf(tiles), answered)
	}

	// Named, the space moves the tiles, and is recorded.
	ad, tiles, _ = install(map[string]any{"proxyUrl": addr, "space": "apps"})
	if ad.Space != "apps" || spaceOf(tiles) != "apps" {
		t.Errorf("installed into apps: addon space %q, tile in %q", ad.Space, spaceOf(tiles))
	}
	ad, tiles, _ = install(map[string]any{"proxyUrl": addr})
	if ad.Space != "apps" || spaceOf(tiles) != "apps" {
		t.Errorf("refreshed after the move: addon space %q, tile in %q", ad.Space, spaceOf(tiles))
	}

	// A space that does not exist: refused, nothing written.
	before := len(fake.installs)
	if rec := postInstall(t, a, map[string]any{"proxyUrl": addr, "space": "nowhere"}); rec.Code != http.StatusBadRequest {
		t.Errorf("install into a missing space = %d %s, want 400", rec.Code, rec.Body)
	}
	if len(fake.installs) != before {
		t.Error("a refused install wrote something")
	}

	// The recorded space is gone: the first space takes the tiles.
	fake.addons["example"] = model.Addon{Key: "example", Space: "gone"}
	if ad, tiles, _ = install(map[string]any{"proxyUrl": addr}); ad.Space != "apps" || spaceOf(tiles) != "apps" {
		t.Errorf("refreshed with its space gone: addon space %q, tile in %q, want apps", ad.Space, spaceOf(tiles))
	}
}

// A chart addon registered again — a new chart version, a new manifest —
// stays in the space it was registered into.
func TestInstallSpaceIsTheRecordedOne(t *testing.T) {
	fake := newFakeStore()
	fake.spaces["media"] = model.Space{Key: "media", Title: "media", Order: 50}
	a := &API{addons: fake}
	ctx := t.Context()
	if got := a.installSpace(ctx, "notes"); got != "apps" {
		t.Errorf("never installed: %q, want the first space", got)
	}
	fake.addons["notes"] = model.Addon{Key: "notes", Space: "media"}
	if got := a.installSpace(ctx, "notes"); got != "media" {
		t.Errorf("installed into media: %q", got)
	}
	fake.addons["notes"] = model.Addon{Key: "notes"}
	if got := a.installSpace(ctx, "notes"); got != "apps" {
		t.Errorf("nothing recorded: %q, want the first space", got)
	}
}
