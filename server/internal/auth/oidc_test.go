package auth

import (
	"context"
	"net/http"
	"testing"
)

// A disabled verifier (no issuer / AuthDisabled) authorizes as an anonymous
// admin so the registry is usable without an IdP.
func TestDisabledVerifierAuthorizesAsAdmin(t *testing.T) {
	j, err := NewJWTVerifier(context.Background(), "", "chino", false, true)
	if err != nil {
		t.Fatalf("NewJWTVerifier: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "/api/portal/launchpad", nil)
	p, ok := j.verifyBearer(context.Background(), req)
	if !ok {
		t.Fatal("disabled verifier should authorize")
	}
	if !p.HasRole("stube-admin") {
		t.Fatal("disabled verifier should grant admin")
	}
}

// An unreachable issuer must be non-fatal at construction and fail closed on
// bearer verification until discovery succeeds.
func TestUnreachableIssuerFailsClosed(t *testing.T) {
	j, err := NewJWTVerifier(context.Background(), "https://127.0.0.1:1/realms/none", "chino", false, false)
	if err != nil {
		t.Fatalf("unreachable issuer should be non-fatal, got %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "/api/portal/apps", nil)
	req.Header.Set("Authorization", "Bearer something")
	if _, ok := j.verifyBearer(context.Background(), req); ok {
		t.Fatal("should fail closed before discovery completes")
	}
}

func TestHasRole(t *testing.T) {
	p := &Principal{Roles: []string{"a", "stube-admin"}}
	if !p.HasRole("stube-admin") {
		t.Fatal("expected role present")
	}
	if p.HasRole("missing") {
		t.Fatal("unexpected role")
	}
	var nilP *Principal
	if nilP.HasRole("x") {
		t.Fatal("nil principal should not have roles")
	}
}
