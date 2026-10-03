package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// The core entries — the media app, the apps and manage spaces — cannot be
// deleted: one confirm() used to be all that stood between an admin and a
// launchpad without its product, or a space whose delete took every tile in
// it. They can still be edited; core itself is the platform's to say.
func TestCoreEntriesCannotBeDeleted(t *testing.T) {
	e := audienceEnv(t)
	e.store.apps["chino"] = model.App{Key: "chino", Title: "chino", Enabled: true, BaseURL: "/chino/", Core: true}
	for _, key := range []string{"apps", "manage"} {
		sp := e.store.spaces[key]
		sp.Core = true
		e.store.spaces[key] = sp
	}
	e.store.apps["wiki"] = model.App{Key: "wiki", Title: "wiki", Enabled: true}
	e.store.spaces["scratch"] = model.Space{Key: "scratch", Title: "scratch", Audience: []string{}}

	for _, c := range []struct{ path, mention string }{
		{"/api/portal/apps/chino", `app "chino" is a core entry`},
		{"/api/portal/spaces/apps", `space "apps" is a core entry`},
		{"/api/portal/spaces/manage", `space "manage" is a core entry`},
	} {
		rec := e.do(adminPortal, http.MethodDelete, c.path, nil)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), c.mention) {
			t.Errorf("DELETE %s = %d %q, want 409 mentioning %q", c.path, rec.Code, rec.Body, c.mention)
		}
	}
	if _, ok := e.store.apps["chino"]; !ok {
		t.Fatal("the core app is gone")
	}
	if _, ok := e.store.tiles["chino.open"]; !ok {
		t.Fatal("the core app's tile is gone")
	}
	if _, ok := e.store.tiles["katalog.catalog"]; !ok {
		t.Fatal("a core space's tile is gone")
	}

	// Edited — retitled, disabled — it stays core, whatever the body says.
	rec := e.do(adminPortal, http.MethodPatch, "/api/portal/apps/chino", map[string]any{"title": "media", "enabled": false, "core": false})
	if rec.Code != http.StatusOK || !e.store.apps["chino"].Core || e.store.apps["chino"].Title != "media" {
		t.Errorf("PATCH = %d %s, app %+v", rec.Code, rec.Body, e.store.apps["chino"])
	}
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/apps/chino", nil); rec.Code != http.StatusConflict {
		t.Errorf("after the PATCH, DELETE = %d", rec.Code)
	}
	// A new app that calls itself core is not one.
	e.do(adminPortal, http.MethodPost, "/api/portal/apps", map[string]any{"key": "mine", "title": "mine", "core": true})
	if e.store.apps["mine"].Core {
		t.Error("a write made an app core")
	}

	// Everything else is deleted as before.
	for _, p := range []string{"/api/portal/apps/wiki", "/api/portal/spaces/scratch", "/api/portal/apps/mine"} {
		if rec := e.do(adminPortal, http.MethodDelete, p, nil); rec.Code != http.StatusNoContent {
			t.Errorf("DELETE %s = %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/spaces/none", nil); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of nothing = %d", rec.Code)
	}
}
