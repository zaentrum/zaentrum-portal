package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// seen is what an embedded app received through the proxy.
type seen struct {
	path, auth string
}

// proxyEnv registers an app "sample" whose in-cluster address is a backend
// that records every request it gets.
func proxyEnv(t *testing.T) (*tokenEnv, func() []seen) {
	t.Helper()
	e := newTokenEnv(t)
	var (
		mu  sync.Mutex
		got []seen
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{path: r.URL.Path, auth: r.Header.Get("Authorization")})
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	e.store.apps["sample"] = model.App{Key: "sample", Title: "sample", Enabled: true, ProxyURL: backend.URL}
	return e, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
}

// The browser loads an addon's console with a plain import() and <link>,
// which carry no bearer: those paths — and the capability manifest — stay
// open, and reach the addon without any Authorization header.
func TestProxyServesThePublicBundleToAnyone(t *testing.T) {
	e, received := proxyEnv(t)
	for _, p := range []string{
		"/api/portal/apps/sample/embed/assets/remoteEntry.js",
		"/api/portal/apps/sample/embed/assets/console.css",
		"/api/portal/apps/sample/embed/assets/__federation_expose_Console-RGXPuZSW.js",
		"/api/portal/apps/sample/.well-known/zaentrum-capability.json",
	} {
		if rec := e.do(nil, http.MethodGet, p, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s without a bearer = %d %s", p, rec.Code, rec.Body)
		}
	}
	if got := received(); len(got) != 4 || got[0].path != "/embed/assets/remoteEntry.js" {
		t.Errorf("the app received %+v", got)
	}
	// A bearer sent along anyway is not forwarded: the bundle needs none.
	e.do(viewerMedia, http.MethodGet, "/api/portal/apps/sample/embed/assets/remoteEntry.js", nil)
	if got := received(); len(got) != 1 || got[0].auth != "" {
		t.Errorf("the public bundle received %+v", got)
	}
}

// Everything else an app serves through the portal takes a signed-in user —
// any client's, since chino's actions reach addon APIs here with its user's
// token — and the request reaches the app with that bearer.
func TestProxyTakesASignedInUserForTheRest(t *testing.T) {
	e, received := proxyEnv(t)
	for _, p := range []string{
		"/api/portal/apps/sample/api/hello",
		"/api/portal/apps/sample/",
		"/api/portal/apps/sample/api/setup",
		// The dot segments are resolved before the path is judged: these are
		// /api/secret, not part of the bundle.
		"/api/portal/apps/sample/embed/../api/secret",
		"/api/portal/apps/sample/embed/%2e%2e/api/secret",
		"/api/portal/apps/sample/.well-known/../api/secret",
		"/api/portal/apps/sample/embedded/x", // a prefix is not the path
	} {
		if rec := e.do(nil, http.MethodGet, p, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a bearer = %d, want 401", p, rec.Code)
		}
	}
	if got := received(); len(got) != 0 {
		t.Fatalf("the app was reached without a signed-in user: %+v", got)
	}
	for name, claims := range map[string]map[string]any{"viewer through the media app": viewerMedia, "admin through the portal": adminPortal} {
		if rec := e.do(claims, http.MethodPost, "/api/portal/apps/sample/api/echo", `{}`); rec.Code != http.StatusOK {
			t.Errorf("POST as %s = %d %s", name, rec.Code, rec.Body)
		}
	}
	got := received()
	if len(got) != 2 || got[0].path != "/api/echo" || got[0].auth == "" {
		t.Errorf("the app received %+v — the user's bearer, at its own path", got)
	}
	// What is sent is what was judged: the resolved path.
	e.do(viewerMedia, http.MethodGet, "/api/portal/apps/sample/embed/../api/secret", nil)
	if got := received(); len(got) != 1 || got[0].path != "/api/secret" || got[0].auth == "" {
		t.Errorf("the app received %+v", got)
	}
}
