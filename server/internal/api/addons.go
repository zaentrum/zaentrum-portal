package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
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
//
// An addon is a group of workloads: one primary component that serves the
// manifest, plus the components it declares. The platform records which
// workloads belong to which addon and shows their live state and the addon's
// setup state. It never stores the addon's configuration and never deploys
// anything — the installer's deployment channel runs the workloads.

// addonTilePrefix marks tiles the platform created for an addon: an addon owns
// the tile keyed addon.<key> and every tile keyed addon.<key>.<something>.
const addonTilePrefix = "addon."

// ownsTile answers whether a tile key belongs to an addon. The dot matters:
// "example" must not claim "addon.example2".
func ownsTile(tileKey, addonKey string) bool {
	base := addonTilePrefix + addonKey
	return tileKey == base || strings.HasPrefix(tileKey, base+".")
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
		Target      string `json:"target"` // e.g. "#/items"; relative to the addon's console
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

// ManifestComponent is one entry of a descriptor's optional `components`: a
// workload the addon consists of. Workload is the Deployment and Service name
// in the addon's namespace — the one fact the console matches live state by.
type ManifestComponent struct {
	Name     string   `json:"name"`
	Workload string   `json:"workload"`
	Role     string   `json:"role"` // primary | required | optional
	Summary  string   `json:"summary,omitempty"`
	Topics   []string `json:"topics,omitempty"` // topics this component emits
}

// ManifestSetup is a descriptor's optional `setup`: the addon's own endpoint
// that reports whether it is configured, and the sections that report covers.
// The platform renders the answer and links to where each section is edited;
// it never sees a configuration value.
type ManifestSetup struct {
	Path     string                 `json:"path"` // service-relative GET, reached through the app proxy
	Sections []ManifestSetupSection `json:"sections,omitempty"`
}

// ManifestSetupSection is one checklist entry.
type ManifestSetupSection struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	Target      string `json:"target,omitempty"` // where it is configured, inside the addon's console
	Ord         int    `json:"ord"`
}

// Manifest limits. Generous for a real addon, small enough that a descriptor
// cannot turn a settings page into something unrenderable.
const (
	maxComponents       = 16
	maxSetupSections    = 16
	maxComponentSummary = 120
	maxSectionTitle     = 60
)

// Component roles.
const (
	rolePrimary  = "primary"
	roleRequired = "required"
	roleOptional = "optional"
)

// addonPlan is what installing a manifest produces. Pure data, so the
// transformation is testable without a database.
type addonPlan struct {
	App        model.App
	Space      *model.Space // nil unless the addon asks for its own launchpad section
	Tiles      []model.Tile // empty when the addon contributes no launchpad entry
	Rows       []model.Extension
	Components []model.AddonComponent // never empty: an addon has at least its primary
	Topics     map[string][]string    // component name → topics it declares
	Setup      *ManifestSetup         // normalised; nil when the addon reports no setup
	Version    string
	Manifest   []byte // the descriptor, canonically encoded
	SHA256     string // of Manifest
}

// manifestError is a descriptor the platform refuses: the addon answered, but
// what it declared breaks the contract. The admin sees exactly which field.
type manifestError struct{ msg string }

func (e *manifestError) Error() string { return "the addon's manifest is invalid: " + e.msg }

func invalid(format string, args ...any) error {
	return &manifestError{msg: fmt.Sprintf(format, args...)}
}

var errNoService error = &manifestError{msg: "descriptor does not name a service"}

// dnsLabel is an RFC 1123 label: what a Deployment or Service name may be
// when it must also be a single DNS label (no dots).
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func isDNSLabel(s string) bool { return len(s) <= 63 && dnsLabel.MatchString(s) }

// schemePrefix matches "javascript:", "https:" and the like at the start of a
// target — anything that would leave the addon's console.
var schemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// validTarget is the rule for anything that opens a place INSIDE an addon's
// console — tile targets and setup section targets alike: a hash route, a
// query, or a relative path. Never a scheme, never another host, never a
// path that climbs out of /portal/app/<key>.
func validTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	if hasControl(target) || strings.ContainsRune(target, '\\') {
		return errors.New("contains control characters or backslashes")
	}
	if strings.HasPrefix(target, "//") || strings.Contains(target, "://") || schemePrefix.MatchString(target) {
		return errors.New("must stay inside the addon's console (no scheme, no host)")
	}
	path := target
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	decoded, err := decodePath(path)
	if err != nil {
		return err
	}
	for _, seg := range strings.Split(decoded, "/") {
		if seg == ".." {
			return errors.New("must not contain '..'")
		}
	}
	return nil
}

// validSetupPath is the rule for setup.path: a relative GET path on the addon
// itself, which the browser reaches through the portal's app proxy.
func validSetupPath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return errors.New("must start with '/'")
	case strings.Contains(p, "//"):
		return errors.New("must not contain '//'")
	case strings.Contains(p, ".."):
		return errors.New("must not contain '..'")
	case strings.Contains(p, "://") || schemePrefix.MatchString(p):
		return errors.New("must not carry a scheme")
	case hasControl(p) || strings.ContainsAny(p, "\\# "):
		return errors.New("must not contain spaces, fragments, backslashes or control characters")
	}
	path := p
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	decoded, err := decodePath(path)
	if err != nil {
		return err
	}
	if strings.Contains(decoded, "..") {
		return errors.New("must not contain '..'")
	}
	return nil
}

// decodePath is the path part of a target or setup path as a browser resolves
// it. A browser treats "%2e%2e", ".%2e" and "%2E." as "..", so a path that
// only spells its dots encoded still climbs out of /portal/app/<key> — or out
// of the app proxy, with the admin's bearer attached. Callers check the
// decoded form. An encoded '/' or '\' is refused outright: a proxy may turn
// it back into a separator.
func decodePath(path string) (string, error) {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return "", errors.New("must not contain an encoded '/' or '\\'")
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", errors.New("contains an invalid percent-escape")
	}
	return decoded, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// installHost is the workload an install address names: the first DNS label
// of its host, which is the Service name whether the admin typed
// http://example, http://example:8080 or http://example.ns.svc.cluster.local.
// An IP address names no Service, so it names no workload: "".
func installHost(proxyURL string) string {
	u, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return ""
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i]
	}
	return host
}

// sameAddress answers whether two install addresses reach the same place:
// scheme and host compared without case, an explicit default port equal to
// none, a trailing slash ignored.
func sameAddress(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return strings.TrimRight(s, "/")
		}
		scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
		if port != "" {
			host = net.JoinHostPort(host, port)
		}
		return scheme + "://" + host + strings.TrimRight(u.Path, "/")
	}
	return norm(a) == norm(b)
}

// planComponents validates the declared components, or synthesises the
// implicit one: an addon that declares none is exactly its primary workload,
// the one the admin typed the address of. The implicit component obeys the
// rules a declared one does — its name is the service, so the service must be
// a DNS-1123 label.
func planComponents(d Descriptor, service, host string) ([]model.AddonComponent, map[string][]string, error) {
	if len(d.Components) == 0 {
		if !isDNSLabel(service) {
			return nil, nil, invalid("service %q must be a DNS-1123 label when the manifest declares no components — it names the implicit primary component", service)
		}
		return []model.AddonComponent{{Name: service, Workload: host, Role: rolePrimary}}, nil, nil
	}
	if len(d.Components) > maxComponents {
		return nil, nil, invalid("components: at most %d, got %d", maxComponents, len(d.Components))
	}
	var (
		out       []model.AddonComponent
		names     = map[string]bool{}
		workloads = map[string]bool{}
		primaries []string
	)
	for i, c := range d.Components {
		name, workload, role := strings.TrimSpace(c.Name), strings.TrimSpace(c.Workload), strings.TrimSpace(c.Role)
		switch {
		case !isDNSLabel(name):
			return nil, nil, invalid("components[%d].name %q must be a DNS-1123 label", i, c.Name)
		case names[name]:
			return nil, nil, invalid("components[%d].name %q is declared twice", i, name)
		case !isDNSLabel(workload):
			return nil, nil, invalid("components[%d].workload %q must be a DNS-1123 label (a Deployment and Service name, no dots)", i, c.Workload)
		case workloads[workload]:
			return nil, nil, invalid("components[%d].workload %q is declared twice", i, workload)
		case role != rolePrimary && role != roleRequired && role != roleOptional:
			return nil, nil, invalid("components[%d].role %q must be primary, required or optional", i, c.Role)
		case utf8.RuneCountInString(c.Summary) > maxComponentSummary:
			return nil, nil, invalid("components[%d].summary is longer than %d characters", i, maxComponentSummary)
		}
		names[name], workloads[workload] = true, true
		if role == rolePrimary {
			primaries = append(primaries, workload)
		}
		out = append(out, model.AddonComponent{
			Name: name, Workload: workload, Role: role, Summary: strings.TrimSpace(c.Summary), Order: i,
		})
	}
	if len(primaries) != 1 {
		return nil, nil, invalid("components: exactly one primary is required, got %d", len(primaries))
	}
	// The primary is the workload that served this manifest. Anything else
	// would let an addon claim to be something the admin did not point at.
	if primaries[0] != host {
		return nil, nil, invalid("components: the primary workload %q is not the install address host %q", primaries[0], host)
	}
	return out, componentTopics(d), nil
}

// componentTopics maps component name → the topics it declares. Topics are
// not stored in rows; they are read back from the manifest.
func componentTopics(d Descriptor) map[string][]string {
	out := map[string][]string{}
	for _, c := range d.Components {
		for _, t := range c.Topics {
			if t = strings.TrimSpace(t); t != "" {
				name := strings.TrimSpace(c.Name)
				out[name] = append(out[name], t)
			}
		}
	}
	return out
}

// normaliseSetup validates setup and returns it with defaults applied and
// sections in display order. Nil in, nil out.
func normaliseSetup(s *ManifestSetup) (*ManifestSetup, error) {
	if s == nil {
		return nil, nil
	}
	out := &ManifestSetup{Path: strings.TrimSpace(s.Path)}
	if err := validSetupPath(out.Path); err != nil {
		return nil, invalid("setup.path %q %v", s.Path, err)
	}
	if len(s.Sections) > maxSetupSections {
		return nil, invalid("setup.sections: at most %d, got %d", maxSetupSections, len(s.Sections))
	}
	keys := map[string]bool{}
	for i, sec := range s.Sections {
		sec.Key, sec.Title, sec.Target = strings.TrimSpace(sec.Key), strings.TrimSpace(sec.Title), strings.TrimSpace(sec.Target)
		sec.Description = strings.TrimSpace(sec.Description)
		switch {
		case !isDNSLabel(sec.Key):
			return nil, invalid("setup.sections[%d].key %q must be a DNS-1123 label", i, sec.Key)
		case keys[sec.Key]:
			return nil, invalid("setup.sections[%d].key %q is declared twice", i, sec.Key)
		case utf8.RuneCountInString(sec.Title) > maxSectionTitle:
			return nil, invalid("setup.sections[%d].title is longer than %d characters", i, maxSectionTitle)
		}
		if err := validTarget(sec.Target); err != nil {
			return nil, invalid("setup.sections[%d].target %q %v", i, sec.Target, err)
		}
		keys[sec.Key] = true
		if sec.Title == "" {
			sec.Title = sec.Key
		}
		out.Sections = append(out.Sections, sec)
	}
	sort.SliceStable(out.Sections, func(i, j int) bool { return out.Sections[i].Ord < out.Sections[j].Ord })
	return out, nil
}

// canonicalManifest encodes a descriptor the same way wherever it was decoded,
// so the hash of what was installed is comparable with the hash of what the
// addon serves now.
func canonicalManifest(d Descriptor) ([]byte, string) {
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

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
	// The primary workload is the host of the address, so the host must be a
	// Service name. Otherwise http://[fe80::1] would record a workload no
	// Deployment can have, and http://localhost:8081 and :8082 one workload
	// two addons then fight over.
	host := installHost(proxyURL)
	if !isDNSLabel(host) {
		return addonPlan{}, fmt.Errorf(
			"the install address %q must name the addon's primary Service — its host a DNS-1123 label, not an IP address", proxyURL)
	}
	components, topics, err := planComponents(d, key, host)
	if err != nil {
		return addonPlan{}, err
	}
	setup, err := normaliseSetup(d.Setup)
	if err != nil {
		return addonPlan{}, err
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
	manifest, sum := canonicalManifest(d)
	plan := addonPlan{
		App: model.App{
			Key: key, Title: title, Description: desc, Kind: "tool", Icon: icon, Enabled: true,
			BaseURL: "/portal/app/" + key, ProxyURL: proxyURL,
		},
		Components: components, Topics: topics, Setup: setup,
		Version: d.Version, Manifest: manifest, SHA256: sum,
	}
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
		for i, t := range ui.Tiles {
			local := strings.TrimSpace(t.Key)
			if local == "" || t.Title == "" {
				continue // a tile with no identity or nothing to say
			}
			if err := validTarget(t.Target); err != nil {
				return addonPlan{}, invalid("ui.tiles[%d].target %q %v", i, t.Target, err)
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

// install is the plan as the store writes it.
func (p addonPlan) install() store.AddonInstall {
	return store.AddonInstall{
		App: p.App, Space: p.Space, Tiles: p.Tiles, Rows: p.Rows,
		Addon: model.Addon{
			Key: p.App.Key, Address: p.App.ProxyURL, Version: p.Version,
			Manifest: p.Manifest, ManifestSHA256: p.SHA256,
		},
		Components: p.Components,
	}
}

// trimLeadingSlash joins a tile target to the addon's console base without
// doubling the separator. A target is a path or hash INSIDE the console
// ("#/items", "/items"), never somewhere else.
func trimLeadingSlash(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "?") {
		return target
	}
	return "/" + strings.TrimLeft(target, "/")
}

// conflictError is an install the registry refuses although the manifest is
// valid: something by that name already exists and is not the addon's.
type conflictError struct{ msg string }

func (e *conflictError) Error() string { return e.msg }

// registered is what the registry already holds under a plan's key.
type registered struct {
	app   *model.App   // nil when no app has the key
	addon *model.Addon // nil when the key was never installed as an addon
}

// adopts answers whether an install takes over an existing app that has no
// addon record: an addon installed before the addons table that left no tile
// and no slot row to backfill it by. It is recognisable by what an install
// wrote — the same proxy address and the addon base URL — so installing it
// again at that address refreshes it, as it always did. Any other app of that
// key was registered by hand and stays a conflict.
func (r registered) adopts(p addonPlan) bool {
	return r.app != nil && r.addon == nil &&
		sameAddress(r.app.ProxyURL, p.App.ProxyURL) && r.app.BaseURL == p.App.BaseURL
}

// movedFrom is the address an installed addon is recorded at when the plan
// installs it from a different one; "" when nothing moves.
func (r registered) movedFrom(p addonPlan) string {
	if r.addon == nil || r.addon.Address == "" || sameAddress(r.addon.Address, p.App.ProxyURL) {
		return ""
	}
	return r.addon.Address
}

// installConflict checks the four things an install must never take:
//
//   - an app of the same key that was not installed as an addon — installing
//     would silently turn a hand-registered app into an addon and let removal
//     delete it;
//   - an installed addon's address, unless the admin confirmed the move — any
//     address serving a manifest with the same service would otherwise
//     redirect the addon's proxy, and every bearer it forwards, elsewhere;
//   - a platform workload — an addon declaring the platform's own Deployment
//     as its component would make the console, and its removal notice, lie;
//   - a workload another addon already declared — one Deployment cannot be
//     two addons' component.
//
// platform is the set of platform-owned workload names (empty when the
// platform cannot see workloads); claims maps workload → owning addon key.
func installConflict(p addonPlan, reg registered, replaceAddress bool, platform map[string]bool, claims map[string]string) error {
	if reg.app != nil && reg.addon == nil && !reg.adopts(p) {
		return &conflictError{msg: fmt.Sprintf(
			"an app with key %q is already registered and was not installed as an addon — remove or rename that app first", p.App.Key)}
	}
	if from := reg.movedFrom(p); from != "" && !replaceAddress {
		return &conflictError{msg: fmt.Sprintf(
			"addon %q is installed from %s — installing it from %s moves it there; check shows the move, and installing must confirm it with replaceAddress",
			p.App.Key, from, p.App.ProxyURL)}
	}
	for _, c := range p.Components {
		if platform[c.Workload] {
			return &conflictError{msg: fmt.Sprintf(
				"component %q names workload %q, which is a platform deployment", c.Name, c.Workload)}
		}
		if owner, ok := claims[c.Workload]; ok && owner != p.App.Key {
			return &conflictError{msg: fmt.Sprintf(
				"component %q names workload %q, which addon %q already declares", c.Name, c.Workload, owner)}
		}
	}
	return nil
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

// ─── live state ──────────────────────────────────────────────────────────────

// componentView is one declared component with the live state of its workload.
// The state fields are null when the workload is not deployed; phase is
// "unknown" (the rest null) when the platform cannot see workloads at all —
// "not deployed" and "cannot tell" are different facts.
type componentView struct {
	Name     string   `json:"name"`
	Workload string   `json:"workload"`
	Role     string   `json:"role"`
	Summary  string   `json:"summary"`
	Topics   []string `json:"topics,omitempty"`
	Phase    *string  `json:"phase"`
	Ready    *int     `json:"ready"`
	Desired  *int     `json:"desired"`
	Restarts *int     `json:"restarts"`
	Reason   *string  `json:"reason"`
}

const phaseUnknown = "unknown"

// componentViews matches components to live workloads by name: workload ==
// operator Instance name. known is false when the operator service is
// unavailable.
func componentViews(cs []model.AddonComponent, topics map[string][]string, live map[string]operator.Instance, known bool) []componentView {
	out := make([]componentView, 0, len(cs))
	for _, c := range cs {
		v := componentView{Name: c.Name, Workload: c.Workload, Role: c.Role, Summary: c.Summary, Topics: topics[c.Name]}
		switch in, ok := live[c.Workload]; {
		case !known:
			v.Phase = ptr(phaseUnknown)
		case ok:
			v.Phase, v.Reason = ptr(in.Phase), ptr(in.Reason)
			v.Ready, v.Desired, v.Restarts = ptr(in.ReadyReplicas), ptr(in.DesiredReplicas), ptr(in.Restarts)
		}
		out = append(out, v)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// liveWorkloads reads the namespace's workloads once for a request. known is
// false when the platform cannot see them (not in a cluster, or the apiserver
// refused).
func (a *API) liveWorkloads(ctx context.Context) (live map[string]operator.Instance, known bool) {
	if a.op == nil || !a.op.Available() {
		return nil, false
	}
	instances, err := a.op.Instances(ctx)
	if err != nil {
		return nil, false
	}
	live = make(map[string]operator.Instance, len(instances))
	for _, in := range instances {
		live[in.Name] = in
	}
	return live, true
}

// platformWorkloads is the set of workload names the platform itself owns.
func platformWorkloads(live map[string]operator.Instance) map[string]bool {
	out := map[string]bool{}
	for name, in := range live {
		if in.Group == "platform" {
			out[name] = true
		}
	}
	return out
}

// ─── handlers ────────────────────────────────────────────────────────────────

// installAddon handles POST /api/portal/addons {proxyUrl, space?, publicBase?, dryRun?, replaceAddress?}.
//
// dryRun answers "what would installing this do" — the plan, the live state of
// each declared workload and any conflict — and writes nothing. A move to
// another address is not refused by a dry run but reported as previousAddress,
// so the admin sees it before confirming it with replaceAddress. A real
// install writes the app, space, tiles, rows, the addon and its components in
// one transaction.
func (a *API) installAddon(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyURL       string `json:"proxyUrl"`
		Space          string `json:"space"`
		PublicBase     string `json:"publicBase"`
		DryRun         bool   `json:"dryRun"`
		ReplaceAddress bool   `json:"replaceAddress"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ProxyURL) == "" {
		http.Error(w, "proxyUrl is required (the addon's in-cluster address, e.g. http://example)", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	proxyURL := strings.TrimSpace(body.ProxyURL)
	d, err := fetchManifest(ctx, proxyURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	space := body.Space
	if space == "" {
		space = a.defaultSpace(ctx)
	}
	origin := strings.TrimSpace(body.PublicBase)
	if origin == "" {
		origin = requestOrigin(r)
	}
	plan, err := planAddon(proxyURL, d, space, origin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	var reg registered
	if reg.app, err = a.addons.GetApp(ctx, plan.App.Key); errors.Is(err, store.ErrNotFound) {
		reg.app = nil
	} else if err != nil {
		serverError(w, err)
		return
	}
	if reg.addon, err = a.addons.GetAddon(ctx, plan.App.Key); errors.Is(err, store.ErrNotFound) {
		reg.addon = nil
	} else if err != nil {
		serverError(w, err)
		return
	}
	claims, err := a.addons.WorkloadClaims(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	live, known := a.liveWorkloads(ctx)
	if err := installConflict(plan, reg, body.ReplaceAddress || body.DryRun, platformWorkloads(live), claims); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	out := map[string]any{
		"key": plan.App.Key, "app": plan.App, "space": plan.Space,
		"tiles": len(plan.Tiles), "slots": len(plan.Rows),
		"commands": len(d.Commands), "checks": len(d.Checks),
		"version":         plan.Version,
		"components":      componentViews(plan.Components, plan.Topics, live, known),
		"setup":           plan.Setup,
		"refresh":         reg.addon != nil || reg.adopts(plan),
		"adopt":           reg.adopts(plan),
		"previousAddress": reg.movedFrom(plan),
		"dryRun":          body.DryRun,
	}
	if body.DryRun {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err := a.addons.InstallAddon(ctx, plan.install()); err != nil {
		if errors.Is(err, store.ErrWorkloadClaimed) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		serverError(w, err)
		return
	}
	invalidateDiscovery() // the CLI surface changed
	writeJSON(w, http.StatusOK, out)
}

// installedAddon is one row of settings → addons.
type installedAddon struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	ProxyURL    string          `json:"proxyUrl"`
	Version     string          `json:"version"`
	InstalledAt time.Time       `json:"installedAt"`
	RefreshedAt time.Time       `json:"refreshedAt"`
	Tiles       int             `json:"tiles"`
	Slots       int             `json:"slots"`
	Components  []componentView `json:"components"`
	Setup       *ManifestSetup  `json:"setup"`
	// RefreshAvailable: the addon now serves a different manifest than the
	// one installed — it was redeployed and its contributions may have moved.
	RefreshAvailable bool `json:"refreshAvailable"`
}

// listAddons handles GET /api/portal/addons: what the addons table records,
// with each component's live state and whether a refresh would change
// anything.
func (a *API) listAddons(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addons, err := a.addons.ListAddons(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	live, known := a.liveWorkloads(ctx)
	served := map[string]string{} // service → sha256 of the manifest it serves now
	if len(addons) > 0 {
		_, descs := a.discover(ctx)
		for _, d := range descs {
			_, sum := canonicalManifest(d)
			served[d.Service] = sum
		}
	}
	out := make([]installedAddon, 0, len(addons))
	for _, ad := range addons {
		row := installedAddon{
			Key: ad.Key, Title: ad.Title, ProxyURL: ad.Address, Version: ad.Version,
			InstalledAt: ad.InstalledAt, RefreshedAt: ad.RefreshedAt,
			Tiles: ad.Tiles, Slots: ad.Rows,
		}
		if row.Title == "" {
			row.Title = ad.Key
		}
		var topics map[string][]string
		if d, ok := storedManifest(ad); ok {
			// Stored manifests were valid when installed; a failure here can
			// only mean a newer contract and is not worth hiding the row for.
			row.Setup, _ = normaliseSetup(d.Setup)
			topics = componentTopics(d)
		}
		row.Components = componentViews(ad.Components, topics, live, known)
		if sum, ok := served[ad.Key]; ok && sum != ad.ManifestSHA256 {
			row.RefreshAvailable = true
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// storedManifest decodes what an install recorded. False for an addon
// backfilled from rows that predate the addons table.
func storedManifest(ad model.Addon) (Descriptor, bool) {
	if len(ad.Manifest) == 0 {
		return Descriptor{}, false
	}
	var d Descriptor
	if err := json.Unmarshal(ad.Manifest, &d); err != nil {
		return Descriptor{}, false
	}
	return d, true
}

// removeAddon handles DELETE /api/portal/addons/{key}: everything the platform
// created for it, by key. Subtraction; the core shows no trace afterwards.
// The workloads are not the platform's to delete — the answer names the ones
// still running so the admin can remove them through their deployment channel.
func (a *API) removeAddon(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// A space goes with the addon only if the addon brought it. Without a
	// stored manifest (installed before the addons table) the older rule
	// applies: whatever space its tiles leave empty.
	declaredSpace := ""
	if ad, err := a.addons.GetAddon(ctx, key); err == nil {
		if d, ok := storedManifest(*ad); ok && d.UI != nil && d.UI.Space != nil {
			declaredSpace = strings.TrimSpace(d.UI.Space.Key)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		serverError(w, err)
		return
	}
	removed, err := a.addons.RemoveAddon(ctx, key, declaredSpace)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	invalidateDiscovery()
	writeJSON(w, http.StatusOK, map[string]any{
		"removed": map[string]any{
			"tiles": removed.Tiles, "rows": removed.Rows, "space": strings.Join(removed.Spaces, ","),
		},
		"remainingWorkloads": remainingWorkloads(removed.Workloads, a.liveWorkloadsOrNil(ctx)),
	})
}

// remainingWorkloads is what the admin still has to remove: the declared
// workloads that are running, or all of them when the platform cannot tell.
func remainingWorkloads(declared []string, live map[string]operator.Instance) []string {
	out := []string{}
	for _, wl := range declared {
		if live == nil {
			out = append(out, wl)
			continue
		}
		if _, ok := live[wl]; ok {
			out = append(out, wl)
		}
	}
	return out
}

func (a *API) liveWorkloadsOrNil(ctx context.Context) map[string]operator.Instance {
	live, known := a.liveWorkloads(ctx)
	if !known {
		return nil
	}
	return live
}

// defaultSpace: the first space by order, so a manifest need not know how an
// instance named its launchpad sections.
func (a *API) defaultSpace(ctx context.Context) string {
	if spaces, err := a.addons.ListSpaces(ctx); err == nil && len(spaces) > 0 {
		return spaces[0].Key
	}
	return "apps"
}
