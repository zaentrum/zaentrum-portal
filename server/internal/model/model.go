// Package model holds the portal registry domain types: the App / Space / Tile
// triple that drives the launchpad, plus the assembled launchpad view.
//
//	App   — a registered web app/backend (the thing you or others register).
//	Space — a launchpad section (a TileGroup).
//	Tile  — a launchpad card opening one action of an app; MANY per app.
package model

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
}

// Space is a launchpad section.
type Space struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Order int    `json:"order"`
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
	Enabled     bool   `json:"enabled"`
}

// LaunchTile is a tile resolved for rendering: its href is computed from the
// owning app's base_url + the tile target, and `disabled` folds in whether the
// tile/app is enabled and whether a destination exists.
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
