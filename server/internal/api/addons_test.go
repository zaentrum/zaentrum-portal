package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	if len(plan.Tiles) != 1 {
		t.Fatalf("console:true must produce exactly one tile, got %d", len(plan.Tiles))
	}
	tile := plan.Tiles[0]
	if tile.Key != "addon.sample" || tile.AppKey != "sample" || tile.SpaceKey != "apps" || tile.Target != "/portal/app/sample" {
		t.Errorf("tile = %+v", tile)
	}
	// The launchpad dims a tile that is not enabled — an installed console
	// must be clickable without a second admin action.
	if !tile.Enabled || tile.Open != "inline" {
		t.Errorf("tile must be enabled and open inline: %+v", tile)
	}
	if plan.Space != nil {
		t.Errorf("no space declared ⇒ none planned, got %+v", *plan.Space)
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
	if len(plan.Tiles) != 0 || len(plan.Rows) != 0 {
		t.Errorf("no ui ⇒ no tiles, no rows; got tiles=%d rows=%d", len(plan.Tiles), len(plan.Rows))
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

// A primary Service that answers with a redirect is refused, not followed:
// the redirect names a place nobody validated.
func TestFetchManifestDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(sampleManifest))
	}))
	defer target.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/latest/meta-data/", http.StatusFound)
	}))
	defer primary.Close()
	addr := strings.Replace(primary.URL, "127.0.0.1", "localhost", 1)

	if _, err := fetchManifest(context.Background(), addr); err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("fetchManifest = %v, want the redirect refused", err)
	}
	if descs := collectDescriptors(context.Background(), []string{addr}); len(descs) != 0 {
		t.Errorf("discovery collected %v through a redirect", descs)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target was reached %d times", n)
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

// An addon with real internal structure declares its own launchpad section and
// several entry points into its console. This is what makes a curated layout
// reproducible from the manifest instead of hand-built per instance.
const richManifest = `{
  "service": "example", "kind": "addon",
  "ui": {
    "app": {"title": "example", "description": "items and queues", "icon": "box"},
    "console": true,
    "space": {"key": "example", "title": "example", "ord": 30},
    "tiles": [
      {"key": "items",  "title": "items",  "icon": "inbox",    "target": "#/items",  "ord": 10},
      {"key": "queue", "title": "queue", "target": "#/queue", "ord": 20},
      {"key": "settings",  "title": "profiles", "target": "settings", "ord": 50},
      {"key": "", "title": "no key"},
      {"key": "nameless", "title": ""}
    ]
  }
}`

func TestPlanAddonWithSpaceAndTiles(t *testing.T) {
	plan, err := planAddon("http://example", decodeManifest(t, richManifest), "apps", "https://media.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Space == nil || plan.Space.Key != "example" || plan.Space.Title != "example" || plan.Space.Order != 30 {
		t.Fatalf("space = %+v", plan.Space)
	}
	// Explicit tiles replace the implicit console one, and the two malformed
	// entries are dropped.
	if len(plan.Tiles) != 3 {
		t.Fatalf("tiles = %d, want 3: %+v", len(plan.Tiles), plan.Tiles)
	}
	for _, tl := range plan.Tiles {
		if tl.SpaceKey != "example" {
			t.Errorf("tile %q must land in the addon's own space, got %q", tl.Key, tl.SpaceKey)
		}
		if !ownsTile(tl.Key, "example") {
			t.Errorf("tile %q is not owned by the addon key", tl.Key)
		}
		if !tl.Enabled {
			t.Errorf("tile %q must be enabled", tl.Key)
		}
	}
	if plan.Tiles[0].Key != "addon.example.items" || plan.Tiles[0].Target != "/portal/app/example#/items" {
		t.Errorf("hash target: %+v", plan.Tiles[0])
	}
	if plan.Tiles[0].Icon != "inbox" {
		t.Errorf("tile icon should be its own: %+v", plan.Tiles[0])
	}
	// A tile without its own icon inherits the app's.
	if plan.Tiles[1].Icon != "box" {
		t.Errorf("tile icon should fall back to the app icon: %+v", plan.Tiles[1])
	}
	// A path target is joined with a slash; a hash target is not.
	if plan.Tiles[2].Target != "/portal/app/example/settings" {
		t.Errorf("path target: %q", plan.Tiles[2].Target)
	}
	// console:true is ignored once explicit tiles exist — no duplicate card.
	for _, tl := range plan.Tiles {
		if tl.Key == "addon.example" {
			t.Error("explicit tiles must replace the implicit console tile, not add to it")
		}
	}
}

func TestOwnsTile(t *testing.T) {
	cases := []struct {
		tile, addon string
		want        bool
	}{
		{"addon.example", "example", true},
		{"addon.example.items", "example", true},
		{"addon.example2", "example", false},       // the dot matters
		{"addon.example2.items", "example", false}, // …in both directions
		{"example.items", "example", false},        // a hand-made tile is not ours
		{"chino.open", "example", false},
	}
	for _, c := range cases {
		if got := ownsTile(c.tile, c.addon); got != c.want {
			t.Errorf("ownsTile(%q, %q) = %v, want %v", c.tile, c.addon, got, c.want)
		}
	}
}
