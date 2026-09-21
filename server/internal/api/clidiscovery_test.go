package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/config"
)

// The aggregator's contract: keep what validates, skip what doesn't, and let
// a broken candidate cost only itself. One valid descriptor among a 404, a
// garbage body, an anonymous one and a dead host must yield exactly one entry.
func TestCollectDescriptorsKeepsOnlyValid(t *testing.T) {
	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wellKnownCapability {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"service":"sample-addon","kind":"addon","version":"1.2.3",
			"commands":[{"name":"list","summary":"list things","method":"GET","path":"/api/things"}],
			"checks":[{"name":"system","path":"/api/health/system"}],
			"topics":["sample.thing.done"]}`)
	}))
	defer valid.Close()

	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>definitely not json</html>")
	}))
	defer garbage.Close()

	// A descriptor that cannot name its service describes nothing.
	anonymous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"kind":"addon"}`)
	}))
	defer anonymous.Close()

	got := collectDescriptors(context.Background(), []string{
		valid.URL, notFound.URL, garbage.URL, anonymous.URL,
		"http://127.0.0.1:1", // dead host: must cost its timeout, not the call
	})
	if len(got) != 1 {
		t.Fatalf("want exactly the one valid descriptor, got %d: %+v", len(got), got)
	}
	d := got[0]
	if d.Service != "sample-addon" || d.Kind != "addon" || len(d.Commands) != 1 || len(d.Checks) != 1 {
		t.Fatalf("descriptor did not survive intact: %+v", d)
	}
}

// No candidates must serialize as an empty list, not null — zae decodes
// services as a slice and "null" is the kind of edge that becomes a panic in
// somebody else's client.
func TestCollectDescriptorsEmptyIsEmptySlice(t *testing.T) {
	got := collectDescriptors(context.Background(), nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty slice, got %#v", got)
	}
}

// discoveryDoc renders the document for one configuration, cache cleared so
// the previous test's answer cannot stand in for this one's.
func discoveryDoc(t *testing.T, cfg config.Config) map[string]any {
	t.Helper()
	invalidateDiscovery()
	t.Cleanup(invalidateDiscovery)
	a := &API{addons: newFakeStore(), cfg: cfg}
	body, _ := a.discover(context.Background())
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("discovery document is not JSON: %v (%s)", err, body)
	}
	return doc
}

// The CLI cannot guess where to sign in: a shared realm registers
// per-instance clients and the issuer may sit under a path prefix. So a
// configured instance says both — and says nothing when there is nothing to
// sign in to, which is what an older portal looks like to a CLI.
func TestCLIDiscoveryAdvertisesAuth(t *testing.T) {
	doc := discoveryDoc(t, config.Config{
		OIDCIssuer:  "https://media.example.org/auth/realms/zaentrum",
		CLIClientID: "zae",
	})
	auth, ok := doc["auth"].(map[string]any)
	if !ok {
		t.Fatalf("want an auth object, got %#v", doc["auth"])
	}
	if auth["issuer"] != "https://media.example.org/auth/realms/zaentrum" || auth["clientId"] != "zae" {
		t.Fatalf("auth does not carry issuer + clientId: %#v", auth)
	}
	// Backwards compatible: everything a v1 client already reads is unchanged.
	if doc["capabilityVersion"] != float64(capabilityVersion) {
		t.Fatalf("capabilityVersion changed: %#v", doc["capabilityVersion"])
	}
	if _, ok := doc["services"].([]any); !ok {
		t.Fatalf("services must stay a list: %#v", doc["services"])
	}

	// An operator may register the client under another name.
	doc = discoveryDoc(t, config.Config{OIDCIssuer: "https://media.example.org/auth/realms/zaentrum", CLIClientID: "zae-cli"})
	if auth, _ := doc["auth"].(map[string]any); auth["clientId"] != "zae-cli" {
		t.Fatalf("PORTAL_CLI_CLIENT_ID ignored: %#v", doc["auth"])
	}
}

func TestCLIDiscoveryOmitsAuthWhenThereIsNothingToSignInTo(t *testing.T) {
	for _, cfg := range []config.Config{
		{}, // no issuer configured
		{OIDCIssuer: "https://media.example.org/auth/realms/zaentrum", AuthDisabled: true}, // dev: everyone is an admin
	} {
		if doc := discoveryDoc(t, cfg); doc["auth"] != nil {
			t.Fatalf("want no auth field for %+v, got %#v", cfg, doc["auth"])
		}
	}
}
