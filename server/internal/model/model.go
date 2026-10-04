// Package model holds the portal registry domain types: the App / Space / Tile
// triple that drives the launchpad, plus the assembled launchpad view.
//
//	App   — a registered web app/backend (the thing you or others register).
//	Space — a launchpad section (a TileGroup).
//	Tile  — a launchpad card opening one action of an app; MANY per app.
package model

import (
	"encoding/json"
	"time"
)

// App is a registered web app/backend.
type App struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	BaseURL     string `json:"baseUrl"`   // e.g. "/katalog/", or an absolute URL
	Kind        string `json:"kind"`      // product|manage|tool|external
	HealthURL   string `json:"healthUrl"` // optional
	Icon        string `json:"icon"`      // lucide name, or "glyph:c" for a brand mark
	Enabled     bool   `json:"enabled"`
	// ProxyURL is the in-cluster address the shell proxies to when this app is
	// embedded (e.g. "http://example"). Empty means the app is link-out only —
	// the portal will not proxy to it.
	ProxyURL string `json:"proxyUrl"`
	// Core: the platform stands on this app, and it cannot be deleted. Set by
	// the platform (migration 011); a write that names it changes nothing.
	Core bool `json:"core"`
}

// Space is a launchpad section.
type Space struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Order int    `json:"order"`
	// Audience is the realm roles that may see the space on the launchpad;
	// empty is everyone signed in. On a write, absent (nil) keeps what is
	// stored.
	Audience []string `json:"audience"`
	// Core: one of the platform's own spaces, which cannot be deleted. Set by
	// the platform (migration 011); a write that names it changes nothing.
	Core bool `json:"core"`
}

// Visible reports whether a caller holding roles may see something meant for
// audience: everyone signed in, when it names no role; else anyone holding
// one of the roles it names.
func Visible(audience, roles []string) bool {
	if len(audience) == 0 {
		return true
	}
	for _, want := range audience {
		for _, have := range roles {
			if want == have {
				return true
			}
		}
	}
	return false
}

// Tile is a launchpad card that opens one action of an App within a Space.
type Tile struct {
	Key         string `json:"key"`
	AppKey      string `json:"appKey"`
	SpaceKey    string `json:"spaceKey"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Target      string `json:"target"` // path within the app, or an absolute url
	Order       int    `json:"order"`
	Badge       string `json:"badge"`
	BadgeTone   string `json:"badgeTone"`
	Status      string `json:"status"` // online|offline|""
	External    bool   `json:"external"`
	Open        string `json:"open"` // inline|newtab|"" (unset -> external decides, else inline)
	Enabled     bool   `json:"enabled"`
	// Audience is the realm roles that may see the tile on the launchpad;
	// empty is everyone signed in. On a write, absent (nil) keeps what is
	// stored.
	Audience []string `json:"audience"`
}

// LaunchTile is a tile resolved for rendering: its href is computed from the
// owning app's base_url + the tile target, `open` is the resolved open mode
// (inline|newtab), and `disabled` folds in whether the tile/app is enabled and
// whether a destination exists.
type LaunchTile struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Href        string `json:"href"`
	Order       int    `json:"order"`
	Badge       string `json:"badge"`
	BadgeTone   string `json:"badgeTone"`
	Status      string `json:"status"`
	External    bool   `json:"external"`
	Open        string `json:"open"` // inline|newtab
	Disabled    bool   `json:"disabled"`
}

// LaunchSpace is a launchpad section with its resolved tiles (ordered).
type LaunchSpace struct {
	Key   string       `json:"key"`
	Title string       `json:"title"`
	Order int          `json:"order"`
	Tiles []LaunchTile `json:"tiles"`
}

// Launchpad is the assembled launchpad: ordered spaces, each with its tiles.
type Launchpad struct {
	Spaces []LaunchSpace `json:"spaces"`
}

// Extension is one addon-contributed UI element for a named slot in a product
// app (e.g. a "request this" button in chino's empty-search slot). The core
// renders it natively but knows nothing of what it does; an uninstalled addon
// deletes its rows and leaves no trace.
type Extension struct {
	Key       string `json:"key"`
	Addon     string `json:"addon"`
	Slot      string `json:"slot"`
	Kind      string `json:"kind"` // link|action
	Label     string `json:"label"`
	Icon      string `json:"icon"`
	URL       string `json:"url"`
	Method    string `json:"method"` // for kind=action; default POST
	StatusURL string `json:"statusUrl"`
	Order     int    `json:"ord"`
	Enabled   bool   `json:"enabled"`
}

// Addon is the registry's record of an installed addon: where it was installed
// from, the manifest that install read, and the workloads it consists of.
// Never any of the addon's configuration — an addon owns its settings; the
// platform only shows where they are edited.
type Addon struct {
	Key     string `json:"key"`
	Address string `json:"address"` // the in-cluster address the manifest was pulled from
	Version string `json:"version"`
	// Manifest is the descriptor as installed, re-encoded canonically so its
	// hash is comparable with a later fetch. Nil for an addon backfilled from
	// rows that predate the addons table.
	Manifest       json.RawMessage  `json:"-"`
	ManifestSHA256 string           `json:"manifestSha256"`
	InstalledAt    time.Time        `json:"installedAt"`
	RefreshedAt    time.Time        `json:"refreshedAt"`
	Components     []AddonComponent `json:"components"`
	// ChartRef and ChartVersion are the Helm chart the operator installed the
	// addon from; empty for an addon added by its address.
	ChartRef     string `json:"chartRef"`
	ChartVersion string `json:"chartVersion"`
	// Space is the space the addon was installed into — the one its tiles go
	// to unless its manifest brings its own — which a refresh that names none
	// reuses. Empty for an addon installed before it was recorded whose
	// space could not be told.
	Space string `json:"space"`

	// Read-side joins, not columns of the addons table.
	Title string `json:"title"`
	Tiles int    `json:"tiles"`
	Rows  int    `json:"rows"`
}

// AddonComponent is one workload an addon declares. Workload is the name of
// its Deployment and Service; the console matches live state by it.
type AddonComponent struct {
	Name     string `json:"name"`
	Workload string `json:"workload"`
	Role     string `json:"role"` // primary|required|optional
	Summary  string `json:"summary"`
	Order    int    `json:"ord"`
}
