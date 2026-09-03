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
	Service  string `json:"service"`
	Kind     string `json:"kind"` // platform | addon
	Version  string `json:"version,omitempty"`
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
}

// discoveryCache: descriptors change on deploys, not per request, and zae may
// be invoked in tight succession (shell completion, scripts). A short TTL
// keeps the fan-out off the hot path without ever being meaningfully stale.
type discoveryCache struct {
	mu      sync.Mutex
	fetched time.Time
	doc     []byte
}

var discCache discoveryCache

const discoveryTTL = 30 * time.Second

// cliDiscovery handles GET /api/portal/cli/discovery. Unauthenticated by the
// same reasoning as the app proxy: zae probes it before any login flow exists,
// and it exposes route metadata of an open-source platform, not data. Every
// endpoint a descriptor names still authenticates itself.
func (a *API) cliDiscovery(w http.ResponseWriter, r *http.Request) {
	discCache.mu.Lock()
	if time.Since(discCache.fetched) < discoveryTTL && discCache.doc != nil {
		doc := discCache.doc
		discCache.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
		return
	}
	discCache.mu.Unlock()

	descs := collectDescriptors(r.Context(), a.capabilityCandidates(r.Context()))
	body, _ := json.Marshal(map[string]any{
		"capabilityVersion": capabilityVersion,
		"services":          descs,
	})

	discCache.mu.Lock()
	discCache.fetched = time.Now()
	discCache.doc = body
	discCache.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
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
	if apps, err := a.st.ListApps(ctx); err == nil {
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
	client := &http.Client{Timeout: 2 * time.Second}
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
