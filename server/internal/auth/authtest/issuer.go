// Package authtest is an OIDC issuer for tests: it serves discovery and a
// JWKS from an httptest server and signs access tokens with the claims a test
// names, so the verifier under test checks real signatures, issuers and
// expiries — the same path a request from the cluster's identity provider
// takes — instead of a stand-in that trusts whatever it is told.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const keyID = "authtest"

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
)

// signingKey is one RSA key for the whole test binary: generating one per
// issuer would cost every test a second.
func signingKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		key = k
	})
	return key
}

// Issuer is a running test issuer.
type Issuer struct {
	URL string
	key *rsa.PrivateKey
}

// New starts an issuer; it stops with the test.
func New(t *testing.T) *Issuer {
	t.Helper()
	k := signingKey(t)
	iss := &Issuer{key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                iss.URL,
			"authorization_endpoint":                iss.URL + "/auth",
			"token_endpoint":                        iss.URL + "/token",
			"jwks_uri":                              iss.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": keyID, "use": "sig", "alg": "RS256",
			"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes()),
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	return iss
}

// Token signs an access token. iss, sub, iat and exp are filled in unless
// claims names them.
func (iss *Issuer) Token(t *testing.T, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	body := map[string]any{
		"iss": iss.URL, "sub": "subject", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		body[k] = v
	}
	return sign(t, iss.key, body)
}

// sign builds an RS256 compact JWS.
func sign(t *testing.T, k *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(sig)
}

// Person is the claims of a person signed in through client with roles.
func Person(client, username string, roles ...string) map[string]any {
	return map[string]any{
		"sub": "user-" + username, "azp": client, "preferred_username": username,
		"realm_access": map[string]any{"roles": roles},
	}
}

// ServiceAccount is the claims of a client's own token from the client
// credentials grant, as Keycloak issues it.
func ServiceAccount(client string, roles ...string) map[string]any {
	return map[string]any{
		"sub": "sa-" + client, "azp": client, "preferred_username": "service-account-" + client,
		"realm_access": map[string]any{"roles": roles},
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
