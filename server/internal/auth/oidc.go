// Package auth provides bearer-JWT authentication for the portal-api: a lazy,
// self-healing OIDC verifier (so the service boots even while the bundled
// Keycloak is still starting) plus realm-role extraction for admin gating.
package auth

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Principal identifies an authenticated caller.
type Principal struct {
	Subject  string
	Username string
	Roles    []string // realm_access.roles
}

// HasRole reports whether the principal carries the named realm role.
func (p *Principal) HasRole(role string) bool {
	if p == nil || role == "" {
		return false
	}
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type ctxKey int

const principalKey ctxKey = 0

// WithPrincipal stores p on the context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the principal stored on the context, if any.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok
}

// claims is the subset of the access-token payload we read.
type claims struct {
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// JWTVerifier validates bearer access tokens against an OIDC issuer's JWKS.
// Issuer + signature + expiry (audience optional). A disabled verifier (no
// issuer, or AuthDisabled) authorizes everything as an anonymous admin so the
// service is usable without an IdP in local/dev.
//
// Discovery is LAZY: attempted once at construction (bounded); on failure the
// constructor does NOT error — it returns a verifier that fails closed (401 on
// protected routes) and retries discovery in the background until the issuer is
// reachable. This survives a simultaneous rollout where Keycloak is still
// booting; an eager, fatal init would crash-loop the service until Keycloak up.
type JWTVerifier struct {
	disabled bool

	issuer string
	cfg    *oidc.Config

	mu       sync.RWMutex
	verifier *oidc.IDTokenVerifier // nil until discovery succeeds
}

// NewJWTVerifier builds a verifier. When disabled is true or issuer is blank,
// every request authorizes as an anonymous admin. Otherwise it attempts OIDC
// discovery once; failure is non-fatal (self-heals in the background).
func NewJWTVerifier(ctx context.Context, issuer, audience string, audienceRequired, disabled bool) (*JWTVerifier, error) {
	if disabled || issuer == "" {
		return &JWTVerifier{disabled: true}, nil
	}
	cfg := &oidc.Config{SkipClientIDCheck: !audienceRequired}
	if audienceRequired {
		cfg.ClientID = audience
	}
	j := &JWTVerifier{issuer: issuer, cfg: cfg}

	if !j.discover(ctx) {
		log.Printf("auth: OIDC issuer %s not reachable yet — serving with auth fail-closed, retrying discovery in the background", issuer)
		go j.retryDiscovery(ctx)
	}
	return j, nil
}

// discover attempts a single bounded OIDC discovery. On success it installs the
// token verifier and returns true. Safe to call concurrently.
func (j *JWTVerifier) discover(ctx context.Context) bool {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(dctx, j.issuer)
	if err != nil {
		return false
	}
	v := provider.Verifier(j.cfg)
	j.mu.Lock()
	j.verifier = v
	j.mu.Unlock()
	return true
}

// retryDiscovery re-attempts discovery with capped exponential backoff until it
// succeeds or ctx is cancelled. Failures are logged (throttled) so a permanent
// misconfig — every bearer request rejected — stays visible in the logs.
func (j *JWTVerifier) retryDiscovery(ctx context.Context) {
	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		attempts++
		if j.discover(ctx) {
			log.Printf("auth: OIDC discovery for %s succeeded after %d retry attempt(s) — bearer authentication active", j.issuer, attempts)
			return
		}
		if attempts <= 3 || backoff >= maxBackoff {
			log.Printf("auth: OIDC discovery for %s still failing (attempt %d) — bearer auth remains fail-closed, retrying", j.issuer, attempts)
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (j *JWTVerifier) tokenVerifier() *oidc.IDTokenVerifier {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.verifier
}

// Disabled reports whether the verifier authorizes everything (no IdP).
func (j *JWTVerifier) Disabled() bool { return j.disabled }

// verifyBearer extracts and validates the Authorization bearer token, returning
// the principal (with realm roles). Before discovery completes it fails closed.
func (j *JWTVerifier) verifyBearer(ctx context.Context, r *http.Request) (*Principal, bool) {
	if j.disabled {
		return &Principal{Subject: "anonymous", Username: "anonymous", Roles: []string{"stube-admin"}}, true
	}
	verifier := j.tokenVerifier()
	if verifier == nil {
		return nil, false // discovery not ready — fail closed
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, false
	}
	raw := strings.TrimSpace(h[len("Bearer "):])
	if raw == "" {
		return nil, false
	}
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, false
	}
	p := &Principal{Subject: tok.Subject}
	var c claims
	if err := tok.Claims(&c); err == nil {
		p.Username = c.PreferredUsername
		p.Roles = c.RealmAccess.Roles
	}
	return p, true
}
