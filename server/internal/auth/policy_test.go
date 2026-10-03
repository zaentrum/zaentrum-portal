package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth/authtest"
)

var testPolicy = Policy{AdminRole: "zaentrum-admin", AddonRole: "zaentrum-addon", AdminClients: []string{"zaentrum-web", "zae"}}

// signedIn verifies a token the test issuer signed, the way a request is
// verified, and applies the policy.
func signedIn(t *testing.T, iss *authtest.Issuer, claims map[string]any) (*Principal, bool) {
	t.Helper()
	j, err := NewJWTVerifier(context.Background(), iss.URL, "", "zaentrum-admin", false, false)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMiddleware(j, testPolicy)
	r := httptest.NewRequest(http.MethodGet, "/api/portal/me", nil)
	r.Header.Set("Authorization", "Bearer "+iss.Token(t, claims))
	return m.authenticate(r)
}

// The admin role counts on the portal's own clients only. Every token the
// realm signs is valid; an admin who signed in to a media app carries the
// role in a token that app received — and that token must not administer the
// platform.
func TestAdminNeedsThePortalsOwnClient(t *testing.T) {
	iss := authtest.New(t)
	for _, c := range []struct {
		name   string
		claims map[string]any
		admin  bool
	}{
		{"admin through the portal", authtest.Person("zaentrum-web", "admin", "zaentrum-admin", "zaentrum-user"), true},
		{"admin through the CLI", authtest.Person("zae", "admin", "zaentrum-admin"), true},
		{"admin through the media app", authtest.Person("chino-web", "admin", "zaentrum-admin", "zaentrum-user"), false},
		{"admin through the TV client", authtest.Person("chino-tv", "admin", "zaentrum-admin"), false},
		{"viewer through the portal", authtest.Person("zaentrum-web", "viewer", "zaentrum-user"), false},
		{"viewer through the media app", authtest.Person("chino-web", "viewer", "zaentrum-user"), false},
		{"a service account with the admin role", authtest.ServiceAccount("ci-bot", "zaentrum-admin"), false},
		{"no client named at all", map[string]any{"preferred_username": "admin", "realm_access": map[string]any{"roles": []string{"zaentrum-admin"}}}, false},
		{"client_id when there is no azp", map[string]any{"client_id": "zae", "realm_access": map[string]any{"roles": []string{"zaentrum-admin"}}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ok := signedIn(t, iss, c.claims)
			if !ok {
				t.Fatal("a token the issuer signed must sign the caller in")
			}
			if p.Admin != c.admin {
				t.Errorf("Admin = %v, want %v (client %q, roles %v)", p.Admin, c.admin, p.Client, p.Roles)
			}
		})
	}
}

// An addon's service account writes its own addon's rows: the addon is its
// client, or the zaentrum_addon claim a shared realm maps onto it. A person
// given the role by mistake is no addon.
func TestAddonKeyIsTheServiceAccountsOwn(t *testing.T) {
	iss := authtest.New(t)
	withClaim := authtest.ServiceAccount("demo-sample-svc", "zaentrum-addon")
	withClaim["zaentrum_addon"] = "sample"
	badClaim := authtest.ServiceAccount("demo-sample-svc", "zaentrum-addon")
	badClaim["zaentrum_addon"] = []string{"sample"}
	genericSA := map[string]any{"sub": "sample", "azp": "sample", "realm_access": map[string]any{"roles": []string{"zaentrum-addon"}}}
	for _, c := range []struct {
		name   string
		claims map[string]any
		addon  string
	}{
		{"named after its addon", authtest.ServiceAccount("sample", "zaentrum-addon"), "sample"},
		{"bound by the zaentrum_addon claim", withClaim, "sample"},
		{"a provider that makes the client its subject", genericSA, "sample"},
		{"a claim that is not a string binds nothing", badClaim, "demo-sample-svc"},
		{"without the addon role", authtest.ServiceAccount("sample"), ""},
		{"a person with the addon role", authtest.Person("chino-web", "alice", "zaentrum-addon"), ""},
		{"a person whose username looks like a service account of another client", map[string]any{
			"azp": "chino-web", "preferred_username": "service-account-sample", "realm_access": map[string]any{"roles": []string{"zaentrum-addon"}},
		}, ""},
		{"a client id that is no DNS label", authtest.ServiceAccount("Sample.Addon", "zaentrum-addon"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ok := signedIn(t, iss, c.claims)
			if !ok {
				t.Fatal("not signed in")
			}
			if p.Addon != c.addon {
				t.Errorf("Addon = %q, want %q", p.Addon, c.addon)
			}
			if p.Admin {
				t.Error("an addon is never an admin")
			}
		})
	}
	// A claim of the wrong type costs the binding, not the roles.
	p, _ := signedIn(t, iss, badClaim)
	if !p.HasRole("zaentrum-addon") {
		t.Errorf("roles = %v", p.Roles)
	}
}

func TestTokensTheIssuerDidNotSignAreRefused(t *testing.T) {
	iss, other := authtest.New(t), authtest.New(t)
	j, err := NewJWTVerifier(context.Background(), iss.URL, "", "zaentrum-admin", false, false)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMiddleware(j, testPolicy)
	expired := authtest.Person("zaentrum-web", "admin", "zaentrum-admin")
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	for name, bearer := range map[string]string{
		"another issuer": other.Token(t, authtest.Person("zaentrum-web", "admin", "zaentrum-admin")),
		"expired":        iss.Token(t, expired),
		"garbage":        "not-a-jwt",
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/portal/me", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		if _, ok := m.authenticate(r); ok {
			t.Errorf("%s: signed in", name)
		}
	}
}

// The refusal says what would fix it: the client, and the clients that count.
func TestRefusalsNameTheClient(t *testing.T) {
	iss := authtest.New(t)
	j, _ := NewJWTVerifier(context.Background(), iss.URL, "", "zaentrum-admin", false, false)
	m := NewMiddleware(j, testPolicy)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	serve := func(h http.Handler, claims map[string]any) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/portal/apps", nil)
		r.Header.Set("Authorization", "Bearer "+iss.Token(t, claims))
		rec := httptest.NewRecorder()
		m.Authn(h).ServeHTTP(rec, r)
		return rec
	}
	rec := serve(m.RequireAdmin(ok), authtest.Person("chino-web", "admin", "zaentrum-admin"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"chino-web"`) || !strings.Contains(rec.Body.String(), "zaentrum-web, zae") {
		t.Errorf("admin through the media app = %d %q", rec.Code, rec.Body)
	}
	if rec := serve(m.RequireAdmin(ok), authtest.Person("zaentrum-web", "viewer", "zaentrum-user")); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "requires the zaentrum-admin role") {
		t.Errorf("viewer = %d %q", rec.Code, rec.Body)
	}
	if rec := serve(m.RequireAdmin(ok), authtest.Person("zaentrum-web", "admin", "zaentrum-admin")); rec.Code != http.StatusNoContent {
		t.Errorf("admin through the portal = %d %q", rec.Code, rec.Body)
	}
	if rec := serve(m.RequireAdminOrAddon(ok), authtest.ServiceAccount("sample", "zaentrum-addon")); rec.Code != http.StatusNoContent {
		t.Errorf("an addon's service account = %d %q", rec.Code, rec.Body)
	}
	if rec := serve(m.RequireAdminOrAddon(ok), authtest.Person("chino-web", "alice", "zaentrum-addon")); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "service account") {
		t.Errorf("a person with the addon role = %d %q", rec.Code, rec.Body)
	}
	if rec := serve(m.RequireAdmin(ok), authtest.ServiceAccount("sample", "zaentrum-addon")); rec.Code != http.StatusForbidden {
		t.Errorf("an addon on an admin route = %d %q", rec.Code, rec.Body)
	}
}

// With authentication disabled the synthetic principal is an admin — the
// no-IdP dev profile — but never an addon.
func TestDisabledVerifierGrantsAdmin(t *testing.T) {
	j, _ := NewJWTVerifier(context.Background(), "", "", "zaentrum-admin", false, true)
	p, ok := NewMiddleware(j, testPolicy).authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !ok || !p.Admin || p.Addon != "" {
		t.Fatalf("principal = %+v", p)
	}
	j, _ = NewJWTVerifier(context.Background(), "", "", "zaentrum-user", false, true)
	if p, _ := NewMiddleware(j, testPolicy).authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); p.Admin {
		t.Error("the synthetic principal is an admin only with the admin role")
	}
}
