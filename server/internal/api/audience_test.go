package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// audienceEnv is the seeded launchpad: chino for everyone, the catalog tiles
// for admins, and a space only operations may see.
func audienceEnv(t *testing.T) *tokenEnv {
	t.Helper()
	e := newTokenEnv(t)
	e.store.apps["chino"] = model.App{Key: "chino", Title: "chino", Enabled: true, BaseURL: "/chino/"}
	e.store.apps["katalog"] = model.App{Key: "katalog", Title: "Catalog", Enabled: true, BaseURL: "/katalog/", Kind: "manage"}
	e.store.spaces["manage"] = model.Space{Key: "manage", Title: "manage", Order: 20, Audience: []string{}}
	e.store.spaces["ops"] = model.Space{Key: "ops", Title: "ops", Order: 30, Audience: []string{"ops"}}
	for _, tl := range []model.Tile{
		{Key: "chino.open", AppKey: "chino", SpaceKey: "apps", Title: "chino", Enabled: true, Audience: []string{}},
		{Key: "katalog.catalog", AppKey: "katalog", SpaceKey: "manage", Title: "Catalog", Order: 10, Enabled: true, Audience: []string{"zaentrum-admin"}},
		{Key: "katalog-manage.open", AppKey: "katalog", SpaceKey: "manage", Title: "Catalog Management", Order: 20, Enabled: true, Audience: []string{"zaentrum-admin"}},
		{Key: "katalog.ops", AppKey: "katalog", SpaceKey: "ops", Title: "ops", Enabled: true, Audience: []string{}},
	} {
		e.store.tiles[tl.Key] = tl
	}
	return e
}

func launchpadKeys(t *testing.T, e *tokenEnv, claims map[string]any) []string {
	t.Helper()
	rec := e.do(claims, http.MethodGet, "/api/portal/launchpad", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("launchpad = %d %s", rec.Code, rec.Body)
	}
	var lp model.Launchpad
	if err := json.Unmarshal(rec.Body.Bytes(), &lp); err != nil {
		t.Fatal(err)
	}
	return tileKeysOf(lp)
}

func tileKeysOf(lp model.Launchpad) []string {
	var out []string
	for _, sp := range lp.Spaces {
		for _, tl := range sp.Tiles {
			out = append(out, sp.Key+"/"+tl.Key)
		}
	}
	return out
}

// The launchpad is filtered server-side by the caller's roles: a viewer gets
// no admin tile — not one the SPA hides, one the API never sends.
func TestLaunchpadShowsWhatTheCallerMaySee(t *testing.T) {
	e := audienceEnv(t)
	if got := launchpadKeys(t, e, viewerMedia); !slices.Equal(got, []string{"apps/chino.open"}) {
		t.Errorf("a viewer sees %v", got)
	}
	if got := launchpadKeys(t, e, viewerPortal); !slices.Equal(got, []string{"apps/chino.open"}) {
		t.Errorf("a viewer through the portal sees %v", got)
	}
	want := []string{"apps/chino.open", "manage/katalog.catalog", "manage/katalog-manage.open"}
	if got := launchpadKeys(t, e, adminPortal); !slices.Equal(got, want) {
		t.Errorf("an admin sees %v, want %v", got, want)
	}
	if got := launchpadKeys(t, e, map[string]any{"azp": "zaentrum-web", "realm_access": map[string]any{"roles": []string{"ops"}}}); !slices.Equal(got, []string{"apps/chino.open", "ops/katalog.ops"}) {
		t.Errorf("operations see %v", got)
	}
}

// An audience is written by admins, and left alone by a write that does not
// name one — an older console, an addon's refresh.
func TestAudienceWrites(t *testing.T) {
	e := audienceEnv(t)
	tile := func(kv ...any) map[string]any {
		m := map[string]any{"appKey": "katalog", "spaceKey": "manage", "title": "Catalog", "enabled": true}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	stored := func(key string) []string { return e.store.tiles[key].Audience }

	// Left out: the stored audience stays, and the answer says which it is.
	rec := e.do(adminPortal, http.MethodPatch, "/api/portal/tiles/katalog.catalog", tile("title", "renamed"))
	if rec.Code != http.StatusOK || !slices.Equal(stored("katalog.catalog"), []string{"zaentrum-admin"}) || !strings.Contains(rec.Body.String(), `"audience":["zaentrum-admin"]`) {
		t.Errorf("PATCH without an audience = %d %s, stored %v", rec.Code, rec.Body, stored("katalog.catalog"))
	}
	// Named: trimmed, each once.
	e.do(adminPortal, http.MethodPatch, "/api/portal/tiles/katalog.catalog", tile("audience", []string{" zaentrum-admin ", "ops", "ops", ""}))
	if got := stored("katalog.catalog"); !slices.Equal(got, []string{"zaentrum-admin", "ops"}) {
		t.Errorf("audience = %v", got)
	}
	// Empty: everyone signed in.
	e.do(adminPortal, http.MethodPatch, "/api/portal/tiles/katalog.catalog", tile("audience", []string{}))
	if got := stored("katalog.catalog"); got == nil || len(got) != 0 {
		t.Errorf("audience = %#v", got)
	}
	if got := launchpadKeys(t, e, viewerMedia); !slices.Contains(got, "manage/katalog.catalog") {
		t.Errorf("opened to everyone, a viewer sees %v", got)
	}
	// What is no role name is refused.
	for _, bad := range [][]string{{"zaentrum admin"}, {"a,b"}, {"x\ny"}, {strings.Repeat("r", 256)}} {
		if rec := e.do(adminPortal, http.MethodPatch, "/api/portal/tiles/katalog.catalog", tile("audience", bad)); rec.Code != http.StatusBadRequest {
			t.Errorf("audience %q = %d", bad, rec.Code)
		}
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = "role" + strings.Repeat("x", i)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/spaces", map[string]any{"key": "big", "title": "big", "audience": many}); rec.Code != http.StatusBadRequest {
		t.Errorf("33 roles = %d", rec.Code)
	}
	// Spaces keep theirs the same way.
	e.do(adminPortal, http.MethodPatch, "/api/portal/spaces/ops", map[string]any{"title": "operations"})
	if got := e.store.spaces["ops"]; got.Title != "operations" || !slices.Equal(got.Audience, []string{"ops"}) {
		t.Errorf("space = %+v", got)
	}
	// Viewers write nothing, whatever the audience.
	if rec := e.do(viewerPortal, http.MethodPatch, "/api/portal/tiles/katalog.catalog", tile("audience", []string{})); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's PATCH = %d", rec.Code)
	}
}
