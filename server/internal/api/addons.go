package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// Addon installation is PULL, not push.
//
// An addon does not register itself. It DECLARES what it contributes — in
// the same capability descriptor the CLI reads — and an admin adds the addon
// in settings by its in-cluster address. portal-api fetches the descriptor
// and creates the app, the tile and the slot rows, all owned by the addon
// key. The addon needs no identity to appear, and removing it is one action:
// everything the platform created for it is deleted by that key.
//
// This is the socket/plug principle applied to installation: the platform
// pulls the plug in. The runtime self-registration API stays for addons that
// change their contributions dynamically; it is no longer the install path.

// addonTilePrefix marks tiles the platform created for an addon, so "installed
// addons" is derivable without a schema change: an addon owns the tile keyed
// addon.<key> and every tile keyed addon.<key>.<something>.
const addonTilePrefix = "addon."

// ownsTile answers whether a tile key belongs to an addon. The dot matters:
// "acquire" must not claim "addon.acquire2".
func ownsTile(tileKey, addonKey string) bool {
	base := addonTilePrefix + addonKey
	return tileKey == base || strings.HasPrefix(tileKey, base+".")
}

// addonOfTile is the inverse, for listing what is installed.
func addonOfTile(tileKey string) string {
	if !strings.HasPrefix(tileKey, addonTilePrefix) {
		return ""
	}
	rest := strings.TrimPrefix(tileKey, addonTilePrefix)
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// ManifestUI is the optional `ui` section of a capability descriptor: what the
// addon contributes to the portal and the product apps. Everything here is
// data the platform materialises into registry rows.
type ManifestUI struct {
	App struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Icon        string `json:"icon"` // lucide name; defaults to "puzzle"
	} `json:"app"`
	// Console: the addon serves a federated console at embed/… and wants a
	// tile that opens it inside the portal. Ignored when Tiles is non-empty —
	// an addon that lists its own tiles has said what it wants.
	Console bool `json:"console"`
	// Space: an optional launchpad section for this addon's tiles. Without it
	// the tiles land in the space the admin chose at install.
	Space *struct {
		Key   string `json:"key"`
		Title string `json:"title"`
		Ord   int    `json:"ord"`
	} `json:"space,omitempty"`
	// Tiles: an addon with real internal structure (a queue, a backlog, a
	// settings area) can place several tiles instead of the single console
	// one. Each opens a path INSIDE the addon's own console, so this is a
	// curated set of entry points, not a mirror of the addon's whole nav.
	Tiles []struct {
		Key         string `json:"key"` // addon-local; stored as addon.<addon>.<key>
		Title       string `json:"title"`
		Description string `json:"description"`
		Icon        string `json:"icon"`
		Target      string `json:"target"` // e.g. "#/requests"; relative to the addon's console
		Ord         int    `json:"ord"`
	} `json:"tiles,omitempty"`
	Slots []struct {
		Key    string `json:"key"` // addon-local; stored as <addon>.<key>
		Slot   string `json:"slot"`
		Kind   string `json:"kind"`
		Label  string `json:"label"`
		Icon   string `json:"icon"`
		URL    string `json:"url"` // relative to the portal origin, {q} etc. allowed
		Method string `json:"method,omitempty"`
		Ord    int    `json:"ord"`
	} `json:"slots"`
}

// addonPlan is what installing a manifest produces. Pure data, so the
// transformation is testable without a database.
type addonPlan struct {
	App   model.App
	Space *model.Space // nil unless the addon asks for its own launchpad section
	Tiles []model.Tile // empty when the addon contributes no launchpad entry
	Rows  []model.Extension
}

var errNoService = errors.New("descriptor does not name a service")

// planAddon turns a fetched descriptor into registry rows. Ownership is by
// construction: the app key IS the addon key, the tile is addon.<key>, every
// slot row carries addon=<key> and a key under <key>. — so uninstall can be
// "everything with this key" and cannot miss.
//
// origin is the portal's public origin (scheme://host). Slot URLs in a manifest
// are relative to the portal ("/portal/app/<key>?q={q}") because an addon does
// not know where it is installed; product apps may live on other hosts and
// render rows as plain links, so the platform absolutises them here, at the
// one moment it knows both.
func planAddon(proxyURL string, d Descriptor, spaceKey, origin string) (addonPlan, error) {
	key := strings.TrimSpace(d.Service)
	if key == "" {
		return addonPlan{}, errNoService
	}
	ui := d.UI
	title, desc, icon := key, "", "puzzle"
	console := false
	if ui != nil {
		if ui.App.Title != "" {
			title = ui.App.Title
		}
		desc = ui.App.Description
		if ui.App.Icon != "" {
			icon = ui.App.Icon
		}
		console = ui.Console
	}
	if desc == "" {
		desc = "addon"
	}
	plan := addonPlan{App: model.App{
		Key: key, Title: title, Description: desc, Kind: "tool", Icon: icon, Enabled: true,
		BaseURL: "/portal/app/" + key, ProxyURL: proxyURL,
	}}
	// An addon's own space, when it asks for one: a section of the launchpad
	// that belongs to it and goes away with it.
	if ui != nil && ui.Space != nil && strings.TrimSpace(ui.Space.Key) != "" {
		sp := model.Space{Key: strings.TrimSpace(ui.Space.Key), Title: ui.Space.Title, Order: ui.Space.Ord}
		if sp.Title == "" {
			sp.Title = sp.Key
		}
		plan.Space = &sp
		spaceKey = sp.Key
	}

	tile := func(k, t, d, ic, target string, ord int) model.Tile {
		return model.Tile{
			Key: k, AppKey: key, SpaceKey: spaceKey,
			Title: t, Description: d, Icon: ic,
			Target: target, Order: ord, Open: "inline", Enabled: true,
		}
	}
	switch {
	case ui != nil && len(ui.Tiles) > 0:
		// Explicit tiles: each opens a path inside the addon's own console.
		for _, t := range ui.Tiles {
			local := strings.TrimSpace(t.Key)
			if local == "" || t.Title == "" {
				continue // a tile with no identity or nothing to say
			}
			ic := t.Icon
			if ic == "" {
				ic = icon
			}
			plan.Tiles = append(plan.Tiles, tile(
				addonTilePrefix+key+"."+local, t.Title, t.Description, ic,
				"/portal/app/"+key+trimLeadingSlash(t.Target), t.Ord))
		}
	case console:
		plan.Tiles = append(plan.Tiles, tile(addonTilePrefix+key, title, desc, icon, "/portal/app/"+key, 900))
	}
	if ui != nil {
		for _, s := range ui.Slots {
			if s.Slot == "" || s.Label == "" {
				continue // a contribution with nowhere to go or nothing to say
			}
			kind := s.Kind
			if kind == "" {
				kind = "link"
			}
			local := s.Key
			if local == "" {
				local = s.Slot
			}
			u := s.URL
			if strings.HasPrefix(u, "/") && origin != "" {
				u = strings.TrimRight(origin, "/") + u
			}
			plan.Rows = append(plan.Rows, model.Extension{
				Key: key + "." + local, Addon: key, Slot: s.Slot, Kind: kind,
				Label: s.Label, Icon: s.Icon, URL: u, Method: s.Method, Order: s.Ord, Enabled: true,
			})
		}
	}
	return plan, nil
}

// trimLeadingSlash joins a tile target to the addon's console base without
// doubling the separator. A target is a path or hash INSIDE the console
// ("#/requests", "/requests"), never somewhere else.
func trimLeadingSlash(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "?") {
		return target
	}
	return "/" + strings.TrimLeft(target, "/")
}

// fetchManifest reads an addon's descriptor from its in-cluster address. The
// address is validated exactly like the embed proxy's target — the request
// never decides where portal-api connects to.
func fetchManifest(ctx context.Context, proxyURL string) (Descriptor, error) {
	target, err := embedTarget(proxyURL)
	if err != nil {
		return Descriptor{}, fmt.Errorf("proxy url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.String(), "/")+wellKnownCapability, nil)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return Descriptor{}, fmt.Errorf("the addon did not answer at %s: %w", proxyURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Descriptor{}, fmt.Errorf("the addon answered %d for its capability descriptor — is it a zaentrum addon?", resp.StatusCode)
	}
	var d Descriptor
	if err := json.NewDecoder(&limitedReader{r: resp.Body, n: 256 << 10}).Decode(&d); err != nil {
		return Descriptor{}, fmt.Errorf("the addon's descriptor is not valid JSON: %w", err)
	}
	return d, nil
}

// requestOrigin is the public origin the install request arrived on — the
// portal's own, since the settings console is what sends it. Behind the
// cluster's router the forwarded headers carry it; a direct call falls back to
// the request host. publicBase in the body overrides both for scripted
// installs made from inside the cluster.
func requestOrigin(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if i := strings.IndexByte(host, ','); i >= 0 {
		host = strings.TrimSpace(host[:i])
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// installAddon handles POST /api/portal/addons {proxyUrl, space?, publicBase?}.
func (a *API) installAddon(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyURL   string `json:"proxyUrl"`
		Space      string `json:"space"`
		PublicBase string `json:"publicBase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ProxyURL) == "" {
		http.Error(w, "proxyUrl is required (the addon's in-cluster address, e.g. http://sample-addon)", http.StatusBadRequest)
		return
	}
	d, err := fetchManifest(r.Context(), strings.TrimSpace(body.ProxyURL))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	space := body.Space
	if space == "" {
		space = a.defaultSpace(r.Context())
	}
	origin := strings.TrimSpace(body.PublicBase)
	if origin == "" {
		origin = requestOrigin(r)
	}
	plan, err := planAddon(strings.TrimSpace(body.ProxyURL), d, space, origin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := a.st.UpsertApp(r.Context(), plan.App); err != nil {
		http.Error(w, "app: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if plan.Space != nil {
		if err := a.st.UpsertSpace(r.Context(), *plan.Space); err != nil {
			http.Error(w, "space: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// Replace, don't merge — for tiles as much as rows: a refresh after the
	// addon dropped a tile must remove it, not leave a dead card behind.
	existing, err := a.st.ListTiles(r.Context())
	if err != nil {
		http.Error(w, "tiles: "+err.Error(), http.StatusInternalServerError)
		return
	}
	keep := map[string]bool{}
	for _, t := range plan.Tiles {
		keep[t.Key] = true
	}
	for _, t := range existing {
		if ownsTile(t.Key, plan.App.Key) && !keep[t.Key] {
			if err := a.st.DeleteTile(r.Context(), t.Key); err != nil {
				http.Error(w, "tile "+t.Key+": "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	for _, t := range plan.Tiles {
		if err := a.st.UpsertTile(r.Context(), t); err != nil {
			http.Error(w, "tile "+t.Key+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// Same rule for slot rows: an uninstall-and-reinstall must not resurrect a
	// button the addon dropped.
	if err := a.st.DeleteExtensionsByAddon(r.Context(), plan.App.Key); err != nil {
		http.Error(w, "rows: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, row := range plan.Rows {
		if err := a.st.UpsertExtension(r.Context(), row); err != nil {
			http.Error(w, "row "+row.Key+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	discCache.mu.Lock()
	discCache.fetched = time.Time{} // the CLI surface changed; drop the cache
	discCache.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"key": plan.App.Key, "app": plan.App, "space": plan.Space,
		"tiles": len(plan.Tiles), "slots": len(plan.Rows),
		"commands": len(d.Commands), "checks": len(d.Checks),
	})
}

// listAddons handles GET /api/portal/addons: apps that own an addon tile.
func (a *API) listAddons(w http.ResponseWriter, r *http.Request) {
	tiles, err := a.st.ListTiles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	apps, _ := a.st.ListApps(r.Context())
	exts, _ := a.st.ListExtensions(r.Context())
	byKey := map[string]model.App{}
	for _, ap := range apps {
		byKey[ap.Key] = ap
	}
	rows := map[string]int{}
	for _, e := range exts {
		rows[e.Addon]++
	}
	type installed struct {
		Key      string `json:"key"`
		Title    string `json:"title"`
		ProxyURL string `json:"proxyUrl"`
		Tiles    int    `json:"tiles"`
		Slots    int    `json:"slots"`
	}
	tileCount := map[string]int{}
	var order []string
	for _, t := range tiles {
		key := addonOfTile(t.Key)
		if key == "" {
			continue
		}
		if tileCount[key] == 0 {
			order = append(order, key)
		}
		tileCount[key]++
	}
	out := []installed{}
	seen := map[string]bool{}
	for _, key := range order {
		seen[key] = true
		ap := byKey[key]
		out = append(out, installed{Key: key, Title: ap.Title, ProxyURL: ap.ProxyURL, Tiles: tileCount[key], Slots: rows[key]})
	}
	// Addons without a console have no tile; find them by owned rows.
	for addon, n := range rows {
		if !seen[addon] && addon != "" {
			if ap, ok := byKey[addon]; ok {
				out = append(out, installed{Key: addon, Title: ap.Title, ProxyURL: ap.ProxyURL, Slots: n})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// removeAddon handles DELETE /api/portal/addons/{key}: everything the platform
// created for it, by key. Subtraction; the core shows no trace afterwards.
func (a *API) removeAddon(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	_ = a.st.DeleteExtensionsByAddon(r.Context(), key)
	tiles, err := a.st.ListTiles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	emptied := map[string]bool{}
	for _, t := range tiles {
		if ownsTile(t.Key, key) {
			if err := a.st.DeleteTile(r.Context(), t.Key); err != nil {
				http.Error(w, "tile "+t.Key+": "+err.Error(), http.StatusInternalServerError)
				return
			}
			emptied[t.SpaceKey] = true
		}
	}
	// A space the addon brought goes with it — but only once nothing else
	// lives there. An admin may have moved their own tiles in.
	for _, t := range tiles {
		if !ownsTile(t.Key, key) {
			delete(emptied, t.SpaceKey)
		}
	}
	for spaceKey := range emptied {
		_ = a.st.DeleteSpace(r.Context(), spaceKey)
	}
	if err := a.st.DeleteApp(r.Context(), key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	discCache.mu.Lock()
	discCache.fetched = time.Time{}
	discCache.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// defaultSpace: the first space by order, so a manifest need not know how an
// instance named its launchpad sections.
func (a *API) defaultSpace(ctx context.Context) string {
	if spaces, err := a.st.ListSpaces(ctx); err == nil && len(spaces) > 0 {
		return spaces[0].Key
	}
	return "apps"
}
