package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth/authtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// extensionsEnv holds two installed addons, sample and other, each with a
// row, and a row an admin made by hand under sample's prefix.
func extensionsEnv(t *testing.T) *tokenEnv {
	t.Helper()
	e := newTokenEnv(t)
	for _, key := range []string{"sample", "other"} {
		e.store.apps[key] = model.App{Key: key, Title: key, Enabled: true}
		e.store.addons[key] = model.Addon{Key: key}
	}
	for _, x := range []model.Extension{
		{Key: "sample.hint", Addon: "sample", Slot: "search.empty", Kind: "link", Label: "sample", URL: "/portal/app/sample", Method: "POST", Enabled: true},
		{Key: "other.hint", Addon: "other", Slot: "search.empty", Kind: "link", Label: "other", URL: "/portal/app/other", Method: "POST", Enabled: true},
		{Key: "sample.handmade", Addon: "", Slot: "search.empty", Kind: "link", Label: "by hand", URL: "/portal/app/sample", Method: "POST", Enabled: true},
	} {
		e.store.extensions[x.Key] = x
	}
	return e
}

func extRow(key string, kv ...any) map[string]any {
	m := map[string]any{"key": key, "slot": "search.empty", "label": "x", "url": "/portal/app/sample?q={q}", "enabled": true}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// An addon's service account changes its own rows and nothing else.
func TestAddonWritesOnlyItsOwnRows(t *testing.T) {
	e := extensionsEnv(t)
	before := e.store.extensions["other.hint"]

	for _, c := range []struct {
		name         string
		method, path string
		body         any
		want         int
		mention      string
	}{
		{"its own new row", http.MethodPost, "/api/portal/extensions", extRow("sample.new"), http.StatusOK, ""},
		{"its own row, addon named", http.MethodPost, "/api/portal/extensions", extRow("sample.named", "addon", "sample"), http.StatusOK, ""},
		{"a row naming another addon", http.MethodPost, "/api/portal/extensions", extRow("sample.x", "addon", "other"), http.StatusForbidden, `addon "other"`},
		{"a row keyed under another addon", http.MethodPost, "/api/portal/extensions", extRow("other.x"), http.StatusForbidden, "keyed sample.<name>"},
		{"a key that only starts like its own", http.MethodPost, "/api/portal/extensions", extRow("samplex.y"), http.StatusForbidden, "keyed sample.<name>"},
		{"overwriting another addon's row", http.MethodPatch, "/api/portal/extensions/other.hint", extRow("", "url", "/portal/app/sample"), http.StatusForbidden, "keyed sample.<name>"},
		{"overwriting a row an admin made", http.MethodPost, "/api/portal/extensions", extRow("sample.handmade"), http.StatusForbidden, "an admin made it"},
		{"deleting another addon's row", http.MethodDelete, "/api/portal/extensions/other.hint", nil, http.StatusForbidden, `addon "other"`},
		{"deleting a row an admin made", http.MethodDelete, "/api/portal/extensions/sample.handmade", nil, http.StatusForbidden, "an admin made it"},
		{"deleting a row that is not there", http.MethodDelete, "/api/portal/extensions/sample.none", nil, http.StatusNotFound, ""},
		{"an action into another addon's API", http.MethodPost, "/api/portal/extensions", extRow("sample.act", "kind", "action", "url", "/api/portal/apps/other/api/x"), http.StatusBadRequest, "its own API"},
		{"an action into its own API", http.MethodPost, "/api/portal/extensions", extRow("sample.act", "kind", "action", "url", "/api/portal/apps/sample/api/x"), http.StatusOK, ""},
		{"deleting its own row", http.MethodDelete, "/api/portal/extensions/sample.hint", nil, http.StatusNoContent, ""},
	} {
		rec := e.do(addonSample, c.method, c.path, c.body)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.mention) {
			t.Errorf("%s: %s %s = %d %q, want %d mentioning %q", c.name, c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()), c.want, c.mention)
		}
	}
	if got := e.store.extensions["other.hint"]; got != before {
		t.Errorf("another addon's row changed: %+v", got)
	}
	if _, ok := e.store.extensions["sample.handmade"]; !ok {
		t.Error("the row an admin made is gone")
	}
	if got := e.store.extensions["sample.new"]; got.Addon != "sample" {
		t.Errorf("a row an addon writes is its own: %+v", got)
	}

	// Its list is its own rows.
	rec := e.do(addonSample, http.MethodGet, "/api/portal/extensions", nil)
	var rows []model.Extension
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	for _, r := range rows {
		if r.Addon != "sample" {
			t.Errorf("an addon lists %q, which is %q's", r.Key, r.Addon)
		}
	}
	if len(rows) != 3 {
		t.Errorf("listed %d rows, want sample.new, sample.named, sample.act", len(rows))
	}
}

// The binding is the token's: a shared realm's per-instance client is bound
// by its zaentrum_addon claim; an addon that is not installed contributes
// nothing; a person with the addon role is no addon.
func TestAddonBinding(t *testing.T) {
	e := extensionsEnv(t)
	bound := authtest.ServiceAccount("demo-sample-svc", "zaentrum-addon")
	bound["zaentrum_addon"] = "sample"
	if rec := e.do(bound, http.MethodPost, "/api/portal/extensions", extRow("sample.claimed")); rec.Code != http.StatusOK {
		t.Errorf("a service account bound by its claim = %d %s", rec.Code, rec.Body)
	}
	ghost := authtest.ServiceAccount("ghost", "zaentrum-addon")
	if rec := e.do(ghost, http.MethodPost, "/api/portal/extensions", extRow("ghost.x")); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not installed") {
		t.Errorf("an addon that is not installed = %d %s", rec.Code, rec.Body)
	}
	person := authtest.Person("chino-web", "alice", "zaentrum-addon")
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/portal/extensions"},
		{http.MethodPost, "/api/portal/extensions"},
		{http.MethodDelete, "/api/portal/extensions/sample.hint"},
	} {
		if rec := e.do(person, c.method, c.path, extRow("chino-web.x")); rec.Code != http.StatusForbidden {
			t.Errorf("a person with the addon role: %s %s = %d", c.method, c.path, rec.Code)
		}
	}
	if _, ok := e.store.extensions["sample.hint"]; !ok {
		t.Error("a refused delete deleted")
	}
}

// An admin manages every row, whoever owns it.
func TestAdminManagesEveryRow(t *testing.T) {
	e := extensionsEnv(t)
	if rec := e.do(adminPortal, http.MethodPatch, "/api/portal/extensions/other.hint", extRow("", "addon", "other", "label", "renamed", "url", "/portal/app/other")); rec.Code != http.StatusOK {
		t.Errorf("admin PATCH = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/extensions/sample.handmade", nil); rec.Code != http.StatusNoContent {
		t.Errorf("admin DELETE = %d %s", rec.Code, rec.Body)
	}
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/extensions", nil)
	var rows []model.Extension
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 2 {
		t.Errorf("an admin lists every row: %d", len(rows))
	}
	// The media app's admin token administers nothing here either.
	if rec := e.do(adminMedia, http.MethodDelete, "/api/portal/extensions/other.hint", nil); rec.Code != http.StatusForbidden {
		t.Errorf("admin through the media app = %d", rec.Code)
	}
}
