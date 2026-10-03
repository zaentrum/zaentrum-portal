package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
)

var (
	errNoProxyTarget     = errors.New("no proxy target configured")
	errBadProxyScheme    = errors.New("proxy target must be http or https")
	errBadProxyHost      = errors.New("proxy target has no host")
	errExternalProxyHost = errors.New("proxy target must be an in-cluster address")
)

// writeErr reports a proxy failure in the same JSON shape as the rest of the API.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// contextWithTimeout bounds a proxied request.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// The launchpad can host an app inside its own shell instead of sending the
// browser away to it. Everything the embedded app loads — its module bundle and
// its API — is fetched through the portal, so the shell stays the single front
// door: one origin, one session, one place where access is decided.
//
// Who may reach an app through here: anyone signed in — the portal verifies
// the bearer before it forwards a byte — except for the app's public bundle,
// which the browser loads with a plain import() and <link> that carry no
// bearer: /embed/… (the hosted console's module, its chunks and its
// stylesheet) and /.well-known/… (its capability manifest). Those two paths
// are open, and reach the app without any Authorization header at all.
//
// Identity is passed through rather than swapped: the caller's bearer has
// already been verified by the portal's middleware, and it is forwarded to the
// app so the app can still apply its OWN authorisation (an addon may distinguish
// admins from ordinary users). Minting a portal service token here would make
// every embedded request look like the portal and quietly erase that
// distinction.
//
// The token is forwarded whole. Narrowing it to what the app needs — a token
// for that app alone, without the portal's admin power — would take a token
// exchange at the identity provider (RFC 8693) and a client of portal-api's
// own to make it with; until then an installed addon is trusted with the
// bearers of the people who use it, as its console, which runs inside the
// shell's page, already is.

// proxyTimeout bounds an embedded app's response. Long-poll style endpoints
// (SSE) are exempt — they are detected by the client's Accept header.
const proxyTimeout = 60 * time.Second

// proxyPrefix is where the proxy is mounted; an app's key follows.
const proxyPrefix = "/api/portal/apps/"

// proxyRest is the path an embedded-app request asks the app for: what follows
// /api/portal/apps/<key>, its dot segments resolved. It is worked out once and
// used twice — to decide whether the path is public and as the path the app
// receives — so the path judged is the path sent: /embed/../api/x is /api/x,
// and takes a signed-in user.
func proxyRest(r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.Path, proxyPrefix+chi.URLParam(r, "key"))
	if rest == "" {
		return "/"
	}
	cleaned := path.Clean("/" + rest)
	if strings.HasSuffix(rest, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

// publicBundle reports whether an app path is part of its public bundle.
func publicBundle(rest string) bool {
	return rest == "/embed" || strings.HasPrefix(rest, "/embed/") || strings.HasPrefix(rest, "/.well-known/")
}

// proxyAuth lets an app's public bundle through as it is and requires a
// signed-in user for everything else the proxy reaches.
func (a *API) proxyAuth(mw *auth.Middleware, next http.Handler) http.Handler {
	signedIn := mw.Authn(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicBundle(proxyRest(r)) {
			next.ServeHTTP(w, r)
			return
		}
		signedIn.ServeHTTP(w, r)
	})
}

// appProxy reverse-proxies /api/portal/apps/{key}/* to the app's in-cluster
// address from the registry.
func (a *API) appProxy(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	app, err := a.reg.GetApp(r.Context(), key)
	if err != nil || app == nil {
		writeErr(w, http.StatusNotFound, "no such app")
		return
	}
	if !app.Enabled {
		writeErr(w, http.StatusNotFound, "app is disabled")
		return
	}
	target, err := embedTarget(app.ProxyURL)
	if err != nil {
		// A link-out app has no proxy target; that is a configuration answer,
		// not a server fault.
		writeErr(w, http.StatusBadGateway, "app is not embeddable: "+err.Error())
		return
	}

	prefix := proxyPrefix + key
	// The mount prefix is stripped: the app is unaware it is embedded and
	// serves from its own root.
	rest := proxyRest(r)
	public := publicBundle(rest)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = singleSlash(target.Path + rest)
			req.URL.RawPath = ""
			req.Host = target.Host
			if public {
				// The public bundle needs no identity, and gets none.
				req.Header.Del("Authorization")
			}
			// The app decides what this user may do, so it needs the user's
			// token — not the portal's identity.
			req.Header.Set("X-Forwarded-Host", r.Host)
			req.Header.Set("X-Forwarded-Proto", schemeOf(r))
			// Tell the app where it is mounted, so anything it generates can be
			// addressed by the browser.
			req.Header.Set("X-Forwarded-Prefix", prefix)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, "app unreachable: "+err.Error())
		},
	}

	// An event stream must not be buffered or cut off by the write timeout.
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		proxy.FlushInterval = -1
		proxy.ServeHTTP(w, r)
		return
	}
	ctx, cancel := contextWithTimeout(r, proxyTimeout)
	defer cancel()
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// embedTarget validates the registry's proxy address. Only an absolute http(s)
// URL is accepted, and only to a host with no explicit port-scanning value:
// this endpoint takes its destination from the database, so it must never
// become a general-purpose relay.
func embedTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errNoProxyTarget
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errBadProxyScheme
	}
	if u.Host == "" {
		return nil, errBadProxyHost
	}
	// In-cluster only: a bare service name, or an explicit .svc address. This
	// keeps a mis-typed (or malicious) registry row from reaching the internet.
	host := u.Hostname()
	if strings.Contains(host, ".") &&
		!strings.HasSuffix(host, ".svc") &&
		!strings.HasSuffix(host, ".svc.cluster.local") &&
		!strings.HasSuffix(host, ".localdomain") &&
		host != "127.0.0.1" && host != "localhost" {
		return nil, errExternalProxyHost
	}
	return u, nil
}

func singleSlash(p string) string {
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if p == "" {
		return "/"
	}
	return p
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	return "http"
}
