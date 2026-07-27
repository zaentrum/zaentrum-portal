package api

import (
	"testing"
)

// The proxy takes its destination from a database row, so the guard is the only
// thing standing between a mis-typed (or hostile) registry edit and an open
// relay out of the cluster.
func TestEmbedTargetRejectsUnsafeDestinations(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"empty (link-out app)", ""},
		{"relative", "/acquire/"},
		{"file scheme", "file:///etc/passwd"},
		{"gopher scheme", "gopher://example.com"},
		{"no host", "http://"},
		{"public internet", "http://example.com"},
		{"public https", "https://evil.example.org/x"},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
	} {
		if _, err := embedTarget(tc.raw); err == nil {
			t.Errorf("%s: %q was accepted, want rejected", tc.name, tc.raw)
		}
	}
}

func TestEmbedTargetAcceptsInClusterAddresses(t *testing.T) {
	for _, raw := range []string{
		"http://acquire",
		"http://acquire:8080",
		"http://acquire.zaentrum-beta.svc",
		"http://acquire.zaentrum-beta.svc.cluster.local:8080",
		"http://localhost:8080",
	} {
		if _, err := embedTarget(raw); err != nil {
			t.Errorf("%q was rejected: %v", raw, err)
		}
	}
}

// The app is unaware it is embedded: it must receive its own root path, not the
// portal's mount prefix.
func TestProxyPathRewrite(t *testing.T) {
	target, err := embedTarget("http://acquire")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/api/portal/apps/acquire"
	for _, tc := range []struct{ in, want string }{
		{prefix + "/", "/"},
		{prefix, "/"},
		{prefix + "/api/wanted", "/api/wanted"},
		{prefix + "/assets/index-abc.js", "/assets/index-abc.js"},
	} {
		rest := tc.in[len(prefix):]
		if rest == "" {
			rest = "/"
		}
		if got := singleSlash(target.Path + rest); got != tc.want {
			t.Errorf("%s -> %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestSingleSlash(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"//api//wanted", "/api/wanted"},
		{"/api/wanted", "/api/wanted"},
		{"", "/"},
	} {
		if got := singleSlash(tc.in); got != tc.want {
			t.Errorf("singleSlash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
