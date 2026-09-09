package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The manifest below is what a console-bearing addon declares. The planner
// must turn it into exactly one app, one tile and one slot row — all owned by
// the addon key — so that uninstall-by-key cannot leave anything behind.
const sampleManifest = `{
  "service": "sample", "kind": "addon", "version": "1.2.3",
  "commands": [{"name":"hello","summary":"say hello","method":"GET","path":"/api/hello"}],
  "checks": [{"name":"system","path":"/api/health/system"}],
  "ui": {
    "app": {"title": "Sample addon", "description": "the reference addon", "icon": "puzzle"},
    "console": true,
    "slots": [
      {"key":"search-hint","slot":"search.empty","kind":"link","label":"Try the sample addon","icon":"puzzle","url":"/portal/app/sample?q={q}","ord":90},
      {"slot":"item.actions","label":"Open in sample","url":"/portal/app/sample?item={id}"},
      {"slot":"", "label":"nowhere"},
      {"slot":"search.empty", "label":""}
    ]
  }
}`

func decodeManifest(t *testing.T, s string) Descriptor {
	t.Helper()
	var d Descriptor
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return d
}

func TestPlanAddonWithConsole(t *testing.T) {
	plan, err := planAddon("http://sample-addon", decodeManifest(t, sampleManifest), "apps", "https://media.example.org")
	if err != nil {
		t.Fatal(err)
	}
	app := plan.App
	if app.Key != "sample" || app.Title != "Sample addon" || app.ProxyURL != "http://sample-addon" || !app.Enabled {
		t.Errorf("app = %+v", app)
	}
	if app.BaseURL != "/portal/app/sample" {
		t.Errorf("app.BaseURL = %q — the portal route is derived from the key", app.BaseURL)
	}
	if plan.Tile == nil {
		t.Fatal("console:true must produce a tile")
	}
	if plan.Tile.Key != "addon.sample" || plan.Tile.AppKey != "sample" || plan.Tile.SpaceKey != "apps" || plan.Tile.Target != "/portal/app/sample" {
		t.Errorf("tile = %+v", *plan.Tile)
	}
	// The launchpad dims a tile that is not enabled — an installed console
	// must be clickable without a second admin action.
	if !plan.Tile.Enabled || plan.Tile.Open != "inline" {
		t.Errorf("tile must be enabled and open inline: %+v", *plan.Tile)
	}
	// Two valid slot rows; the two malformed ones (no slot, no label) are dropped.
	if len(plan.Rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(plan.Rows), plan.Rows)
	}
	r0, r1 := plan.Rows[0], plan.Rows[1]
	if r0.Key != "sample.search-hint" || r0.Addon != "sample" || r0.Slot != "search.empty" || r0.Kind != "link" || r0.Order != 90 || !r0.Enabled {
		t.Errorf("row0 = %+v", r0)
	}
	// A row without an explicit key is keyed by its slot; kind defaults to link.
	if r1.Key != "sample.item.actions" || r1.Kind != "link" || r1.Addon != "sample" {
		t.Errorf("row1 = %+v", r1)
	}
	// Relative slot URLs are absolutised against the portal's public origin:
	// product apps may live on other hosts and render rows as plain links.
	if r0.URL != "https://media.example.org/portal/app/sample?q={q}" {
		t.Errorf("row0.URL = %q", r0.URL)
	}
	for _, r := range plan.Rows {
		if !strings.HasPrefix(r.Key, "sample.") || r.Addon != "sample" {
			t.Errorf("row %q is not owned by the addon key", r.Key)
		}
	}
}

func TestPlanAddonWithoutUI(t *testing.T) {
	// A CLI-only addon (no ui section) still becomes an app — that is what
	// puts it on the discovery candidate list — but gets no tile and no rows.
	plan, err := planAddon("http://tool", decodeManifest(t, `{"service":"tool","kind":"addon"}`), "apps", "https://media.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if plan.App.Key != "tool" || plan.App.Title != "tool" || plan.App.Icon != "puzzle" {
		t.Errorf("app = %+v", plan.App)
	}
	if plan.Tile != nil || len(plan.Rows) != 0 {
		t.Errorf("no ui ⇒ no tile, no rows; got tile=%v rows=%d", plan.Tile != nil, len(plan.Rows))
	}
}

func TestPlanAddonRejectsAnonymousDescriptor(t *testing.T) {
	if _, err := planAddon("http://x", decodeManifest(t, `{"kind":"addon"}`), "apps", ""); err == nil {
		t.Fatal("a descriptor without a service name must not install")
	}
}

func TestFetchManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wellKnownCapability {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleManifest))
	}))
	defer srv.Close()

	d, err := fetchManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if d.Service != "sample" || d.UI == nil || !d.UI.Console || len(d.UI.Slots) != 4 {
		t.Errorf("descriptor = %+v", d)
	}
}

func TestFetchManifestFailures(t *testing.T) {
	notAnAddon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notAnAddon.Close()
	if _, err := fetchManifest(context.Background(), notAnAddon.URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a 404 must be reported with its status, got %v", err)
	}
	// The same guard as the embed proxy: no reaching out of the cluster.
	if _, err := fetchManifest(context.Background(), "http://example.com"); err == nil {
		t.Error("an external host must be refused before any request is made")
	}
	if _, err := fetchManifest(context.Background(), "ftp://sample-addon"); err == nil {
		t.Error("a non-http scheme must be refused")
	}
}

func TestPlanAddonKeepsAbsoluteSlotURLs(t *testing.T) {
	m := `{"service":"ext","ui":{"slots":[{"slot":"search.empty","label":"go","url":"https://elsewhere.svc/x"}]}}`
	plan, err := planAddon("http://ext", decodeManifest(t, m), "apps", "https://media.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Rows[0].URL != "https://elsewhere.svc/x" {
		t.Errorf("absolute URLs must pass through untouched, got %q", plan.Rows[0].URL)
	}
}

func TestRequestOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/portal/addons", nil)
	r.Host = "portal-api"
	if got := requestOrigin(r); got != "http://portal-api" {
		t.Errorf("plain request: %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "media.example.org, router.internal")
	if got := requestOrigin(r); got != "https://media.example.org" {
		t.Errorf("forwarded request: %q", got)
	}
}
