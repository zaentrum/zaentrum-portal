package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CLI capability discovery: the aggregation point that makes the zae CLI
// dynamic. Every service and addon MAY serve a descriptor at
// /.well-known/zaentrum-capability.json describing its commands, checks and
// topics; this endpoint fans out to everything the platform knows is running,
// collects the descriptors that exist, and returns them as one document. The
// CLI compiles in no service names — what it can do against this instance is
// exactly what this endpoint returns.
//
// The candidate list is the union of the two registries that already exist:
// the operator console's instance list (platform services, by Deployment
// name) and the app registry's proxy URLs (addons). Registering an addon's
// app is what registers its CLI surface — one registry, third consumer.
//
// Descriptors are data, never code. Nothing here executes anything a service
// returns; the CLI renders HTTP calls from it and makes them with the user's
// own token against this same origin.

// wellKnownCapability is the per-service path. Versioned with the service
// image on purpose: a deployed service declares its own surface, so the CLI
// can never describe an unshipped platform.
const wellKnownCapability = "/.well-known/zaentrum-capability.json"

// capabilityVersion is the schema major this aggregator speaks.
const capabilityVersion = 1

// Descriptor is capability schema v1 — mirrored by the zae client. Kept
// deliberately boring; see docs/extending/cli.md in the front-door repo for
// the published contract.
type Descriptor struct {
	Service string `json:"service"`
	Kind    string `json:"kind"` // platform | addon
	Version string `json:"version,omitempty"`
	// ProxyKey is the app-registry key the portal proxies this service under
	// (/api/portal/apps/<key>/…), when it is not the service name. The CLI
	// reads it; an install always keys the app by service.
	ProxyKey string `json:"proxyKey,omitempty"`
	Commands []struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
		Method  string `json:"method"`
		Path    string `json:"path"`
		Role    string `json:"role,omitempty"`
	} `json:"commands,omitempty"`
	Checks []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"checks,omitempty"`
	Topics []string `json:"topics,omitempty"`
	// Components are the workloads an addon consists of; Setup is where it
	// reports whether it is configured. Both optional — see addons.go.
	Components []ManifestComponent `json:"components,omitempty"`
	Setup      *ManifestSetup      `json:"setup,omitempty"`
	// UI is what the addon contributes to the portal and product apps; the
	// platform materialises it into registry rows on install (addons.go).
	UI *ManifestUI `json:"ui,omitempty"`
}

// discoveryCache: descriptors change on deploys, not per request, and zae may
// be invoked in tight succession (shell completion, scripts). A short TTL
// keeps the fan-out off the hot path without ever being meaningfully stale.
type discoveryCache struct {
	mu      sync.Mutex
	fetched time.Time
	doc     []byte
	descs   []Descriptor // what doc was built from; settings compares manifests against it
}

var discCache discoveryCache

const discoveryTTL = 30 * time.Second

// cliDiscovery handles GET /api/portal/cli/discovery. Unauthenticated by the
// same reasoning as the app proxy: zae probes it before any login flow exists,
// and it exposes route metadata of an open-source platform, not data. Every
// endpoint a descriptor names still authenticates itself.
func (a *API) cliDiscovery(w http.ResponseWriter, r *http.Request) {
	doc, _ := a.discover(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(doc)
}

// discover returns the aggregate document and the descriptors it was built
// from, out of the cache while it is fresh.
func (a *API) discover(ctx context.Context) ([]byte, []Descriptor) {
	discCache.mu.Lock()
	if time.Since(discCache.fetched) < discoveryTTL && discCache.doc != nil {
		doc, descs := discCache.doc, discCache.descs
		discCache.mu.Unlock()
		return doc, descs
	}
	discCache.mu.Unlock()

	descs := collectDescriptors(ctx, a.capabilityCandidates(ctx))
	doc := map[string]any{
		"capabilityVersion": capabilityVersion,
		"services":          descs,
	}
	// An added field, not a new schema: capabilityVersion stays 1 because
	// nothing a v1 client already reads changed shape. A CLI that predates
	// `auth` ignores it and keeps asking for a bearer in the environment.
	if auth := a.cliAuth(); auth != nil {
		doc["auth"] = auth
	}
	body, _ := json.Marshal(doc)

	discCache.mu.Lock()
	discCache.fetched = time.Now()
	discCache.doc = body
	discCache.descs = descs
	discCache.mu.Unlock()
	return body, descs
}

// cliAuth is how a CLI signs in to THIS instance: the issuer whose tokens the
// API validates, and the client id a CLI should use for the device grant. The
// CLI cannot guess either — a shared realm registers per-instance clients, and
// the issuer may live under a path prefix on this very origin — so the
// instance says both, the way it already tells its own SPA which client it is.
//
// Absent when there is nothing to sign in to: no issuer configured, or auth
// disabled (the dev profile authorizes everyone). A CLI then sees no `auth`
// field, which is exactly what an older portal looks like, and says so rather
// than sending an operator into a login flow that authorizes nothing.
//
// Only public configuration: an issuer URL and a public client id, both of
// which every browser that signs in here already sees.
func (a *API) cliAuth() map[string]string {
	if a.cfg.AuthDisabled || a.cfg.OIDCIssuer == "" {
		return nil
	}
	clientID := a.cfg.CLIClientID
	if clientID == "" {
		clientID = "zae" // config defaults it; a blank override is not a reason to advertise nothing
	}
	return map[string]string{
		"issuer":   a.cfg.OIDCIssuer,
		"clientId": clientID,
	}
}

// invalidateDiscovery drops the cache: an install or removal changed which
// services exist, and the next read must see it.
func invalidateDiscovery() {
	discCache.mu.Lock()
	discCache.fetched = time.Time{}
	discCache.mu.Unlock()
}

// capabilityCandidates enumerates every base URL that might serve a
// descriptor. Both sources are the platform's own state — nothing here comes
// from the request, which is what keeps this endpoint SSRF-proof despite
// fanning out.
func (a *API) capabilityCandidates(ctx context.Context) []string {
	seen := map[string]bool{}
	var out []string
	add := func(base string) {
		base = strings.TrimRight(base, "/")
		if base == "" || seen[base] {
			return
		}
		seen[base] = true
		out = append(out, base)
	}

	// Platform services: the operator console's instance list. Deployment
	// name == Service name in this platform's chart, and the names come from
	// the Kubernetes API, not from users.
	if a.op != nil && a.op.Available() {
		if instances, err := a.op.Instances(ctx); err == nil {
			for _, in := range instances {
				add("http://" + in.Name)
			}
		}
	}

	// Addons: every registered app that carries an in-cluster proxy URL. The
	// same guard the embed proxy uses validates the shape.
	if apps, err := a.addons.ListApps(ctx); err == nil {
		for _, app := range apps {
			if app.ProxyURL == "" || !app.Enabled {
				continue
			}
			if u, err := url.Parse(app.ProxyURL); err == nil && u.Scheme == "http" && u.Host != "" {
				add(u.Scheme + "://" + u.Host)
			}
		}
	}
	return out
}

// collectDescriptors fans out to every candidate and keeps what validates.
// Absence is normal — most services will not declare capabilities, and a
// candidate that is down must cost at most its own timeout, never the
// endpoint's availability.
func collectDescriptors(ctx context.Context, bases []string) []Descriptor {
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: noRedirects}
	var (
		mu  sync.Mutex
		out []Descriptor
		wg  sync.WaitGroup
	)
	for _, base := range bases {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+wellKnownCapability, nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var d Descriptor
			if err := json.NewDecoder(&limitedReader{r: resp.Body, n: 256 << 10}).Decode(&d); err != nil {
				return
			}
			if d.Service == "" {
				return // a descriptor that cannot say who it is describes nothing
			}
			mu.Lock()
			out = append(out, d)
			mu.Unlock()
		}(base)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	if out == nil {
		out = []Descriptor{}
	}
	return out
}

// noRedirects answers a redirect with the redirect itself. Candidates are
// validated where they come from; a redirect would take portal-api somewhere
// nobody validated.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// limitedReader caps how much of a descriptor we will read: a misbehaving
// service must not be able to balloon the aggregate document.
type limitedReader struct {
	r interface{ Read([]byte) (int, error) }
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, http.ErrBodyReadAfterClose
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}
