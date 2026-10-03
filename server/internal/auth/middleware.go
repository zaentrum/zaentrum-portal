package auth

import (
	"fmt"
	"net/http"
	"strings"
)

// Middleware authenticates requests with a bearer JWT. Health endpoints are
// public; everything else requires a valid token (the principal, with realm
// roles and what the policy grants it, is placed on the request context).
type Middleware struct {
	jwt    *JWTVerifier
	policy Policy
}

func NewMiddleware(jwt *JWTVerifier, policy Policy) *Middleware {
	return &Middleware{jwt: jwt, policy: policy}
}

func isPublic(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case strings.HasPrefix(path, "/actuator/health"):
		return true
	}
	return false
}

// Authn returns the authentication middleware.
func (m *Middleware) Authn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if p, ok := m.authenticate(r); ok {
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// authenticate verifies the request's bearer and decides what it may do.
func (m *Middleware) authenticate(r *http.Request) (*Principal, bool) {
	p, ok := m.jwt.verifyBearer(r.Context(), r)
	if !ok {
		return nil, false
	}
	m.policy.grant(p)
	return p, true
}

// RequireAdmin gates a handler on the admin role, carried by a token issued
// to one of the portal's own clients. Must run after Authn (which puts the
// principal on the context).
func (m *Middleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok || !p.Admin {
			http.Error(w, m.refusal(p, false), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdminOrAddon gates a handler on EITHER an admin (the human console)
// OR an addon's service account self-managing its extensions. The handler
// scopes an addon to its own rows (Principal.Addon).
func (m *Middleware) RequireAdminOrAddon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok || (!p.Admin && p.Addon == "") {
			http.Error(w, m.refusal(p, true), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refusal says why a signed-in caller was refused, in the terms that fix it.
// The client id it names comes from a verified token.
func (m *Middleware) refusal(p *Principal, addonToo bool) string {
	switch {
	case p.HasRole(m.policy.AdminRole):
		clients := strings.Join(m.policy.AdminClients, ", ")
		if clients == "" {
			clients = "none are configured: PORTAL_ADMIN_CLIENTS is empty"
		}
		return fmt.Sprintf("forbidden: this token was issued to the client %q — admin requests take tokens issued to the portal's own clients (%s)", p.Client, clients)
	case addonToo && p.HasRole(m.policy.AddonRole):
		return "forbidden: the " + m.policy.AddonRole + " role counts on an addon's service account only — a client-credentials token whose client id, or zaentrum_addon claim, is the addon key"
	case addonToo:
		return "forbidden: requires the " + m.policy.AdminRole + " or " + m.policy.AddonRole + " role"
	default:
		return "forbidden: requires the " + m.policy.AdminRole + " role"
	}
}
