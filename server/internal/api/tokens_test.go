package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/auth/authtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// tokenEnv is portal-api's router behind a real verifier: a test issuer signs
// the tokens, so every rule below is checked against signed tokens of the
// clients the bundled realm has — the portal (zaentrum-web), the CLI (zae),
// the media app (chino-web) — and an addon's service account.
type tokenEnv struct {
	t     *testing.T
	iss   *authtest.Issuer
	api   *API
	store *fakeAddonStore
	kube  *k8sfake.Server
	h     http.Handler
}

func newTokenEnv(t *testing.T) *tokenEnv {
	t.Helper()
	iss := authtest.New(t)
	kube := k8sfake.New(t)
	cfg := config.Config{
		AdminRole: "zaentrum-admin", AddonRole: "zaentrum-addon", AdminClients: []string{"zaentrum-web", "zae"},
		OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", OperatorPlural: "zaentrums", AddonPlural: addonPlural,
	}
	st := newFakeStore()
	op := operator.New(kube.Client("zaentrum"), cfg)
	a := &API{reg: st, addons: st, cfg: cfg, op: op, charts: op, workloads: op}
	a.registration.kick = make(chan struct{}, 1)
	jwt, err := auth.NewJWTVerifier(context.Background(), iss.URL, "", cfg.AdminRole, false, false)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	a.Register(r, auth.NewMiddleware(jwt, auth.Policy{AdminRole: cfg.AdminRole, AddonRole: cfg.AddonRole, AdminClients: cfg.AdminClients}))
	return &tokenEnv{t: t, iss: iss, api: a, store: st, kube: kube, h: r}
}

// The callers every rule is checked against.
var (
	adminPortal  = authtest.Person("zaentrum-web", "admin", "zaentrum-admin", "zaentrum-user")
	adminCLI     = authtest.Person("zae", "admin", "zaentrum-admin", "zaentrum-user")
	adminMedia   = authtest.Person("chino-web", "admin", "zaentrum-admin", "zaentrum-user")
	viewerMedia  = authtest.Person("chino-web", "viewer", "zaentrum-user")
	viewerPortal = authtest.Person("zaentrum-web", "viewer", "zaentrum-user")
	addonSample  = authtest.ServiceAccount("sample", "zaentrum-addon")
)

// do sends a request as claims (nil: no bearer at all).
func (e *tokenEnv) do(claims map[string]any, method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, rdr)
	if claims != nil {
		req.Header.Set("Authorization", "Bearer "+e.iss.Token(e.t, claims))
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// /me answers isAdmin the way the admin routes decide, and names the client:
// an admin who signed in to the media app reads false there.
func TestMeSaysWhetherTheConsoleIsTheCallers(t *testing.T) {
	e := newTokenEnv(t)
	for _, c := range []struct {
		name   string
		claims map[string]any
		admin  bool
		client string
	}{
		{"admin through the portal", adminPortal, true, "zaentrum-web"},
		{"admin through the CLI", adminCLI, true, "zae"},
		{"admin through the media app", adminMedia, false, "chino-web"},
		{"viewer", viewerMedia, false, "chino-web"},
	} {
		rec := e.do(c.claims, http.MethodGet, "/api/portal/me", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: /me = %d %s", c.name, rec.Code, rec.Body)
		}
		var me struct {
			IsAdmin   bool   `json:"isAdmin"`
			Client    string `json:"client"`
			AdminRole string `json:"adminRole"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &me)
		if me.IsAdmin != c.admin || me.Client != c.client || me.AdminRole != "zaentrum-admin" {
			t.Errorf("%s: /me = %s", c.name, rec.Body)
		}
	}
	if rec := e.do(nil, http.MethodGet, "/api/portal/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("/me without a bearer = %d", rec.Code)
	}
}

// Every admin route takes the admin role on a token of the portal's own
// clients only. Admin through the media app, a viewer, an addon: 403.
func TestAdminRoutesTakeOnlyThePortalsClients(t *testing.T) {
	e := newTokenEnv(t)
	// An addon with nowhere to read a manifest from, for /addons/{key}.
	e.store.apps["example"] = model.App{Key: "example", Title: "example"}
	e.store.addons["example"] = model.Addon{Key: "example"}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/portal/apps"},
		{http.MethodGet, "/api/portal/spaces"},
		{http.MethodGet, "/api/portal/tiles"},
		{http.MethodGet, "/api/portal/operator"},
		{http.MethodGet, "/api/portal/addons"},
		{http.MethodGet, "/api/portal/addons/example"},
		{http.MethodGet, "/api/portal/addon-charts"},
		{http.MethodGet, "/api/portal/debug/kafka/topology"},
	}
	for _, rt := range routes {
		for _, c := range []struct {
			name   string
			claims map[string]any
			want   int
		}{
			{"admin through the portal", adminPortal, http.StatusOK},
			{"admin through the CLI", adminCLI, http.StatusOK},
			{"admin through the media app", adminMedia, http.StatusForbidden},
			{"viewer through the media app", viewerMedia, http.StatusForbidden},
			{"viewer through the portal", viewerPortal, http.StatusForbidden},
			{"an addon's service account", addonSample, http.StatusForbidden},
			{"no bearer", nil, http.StatusUnauthorized},
		} {
			if rec := e.do(c.claims, rt.method, rt.path, nil); rec.Code != c.want {
				t.Errorf("%s %s as %s = %d %s, want %d", rt.method, rt.path, c.name, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
			}
		}
	}
	// The refusal names the client, so the admin knows where they signed in.
	rec := e.do(adminMedia, http.MethodGet, "/api/portal/addons", nil)
	if !strings.Contains(rec.Body.String(), `"chino-web"`) {
		t.Errorf("refusal = %q", rec.Body)
	}
}

// What anyone signed in may read stays readable with any client's token —
// above all the slot rows chino-api reads with its user's media-app token.
func TestReadsTakeAnySignedInUser(t *testing.T) {
	e := newTokenEnv(t)
	for _, path := range []string{"/api/portal/launchpad", "/api/portal/me", "/api/portal/slots/search.empty"} {
		for name, claims := range map[string]map[string]any{
			"viewer through the media app": viewerMedia, "viewer through the portal": viewerPortal,
			"admin through the media app": adminMedia, "admin through the portal": adminPortal,
		} {
			if rec := e.do(claims, http.MethodGet, path, nil); rec.Code != http.StatusOK {
				t.Errorf("GET %s as %s = %d %s", path, name, rec.Code, rec.Body)
			}
		}
		if rec := e.do(nil, http.MethodGet, path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a bearer = %d", path, rec.Code)
		}
	}
}
