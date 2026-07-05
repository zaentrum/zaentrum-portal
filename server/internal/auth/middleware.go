package auth

import (
	"net/http"
	"strings"
)

// Middleware authenticates requests with a bearer JWT. Health endpoints are
// public; everything else requires a valid token (the principal, with realm
// roles, is placed on the request context).
type Middleware struct {
	jwt       *JWTVerifier
	adminRole string
}

func NewMiddleware(jwt *JWTVerifier, adminRole string) *Middleware {
	return &Middleware{jwt: jwt, adminRole: adminRole}
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
		if p, ok := m.jwt.verifyBearer(r.Context(), r); ok {
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// RequireAdmin gates a handler on the configured realm admin role. Must run
// after Authn (which puts the principal on the context).
func (m *Middleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok || !p.HasRole(m.adminRole) {
			http.Error(w, "forbidden: requires the "+m.adminRole+" role", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
