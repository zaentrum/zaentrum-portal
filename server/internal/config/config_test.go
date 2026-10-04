package config

import (
	"slices"
	"testing"
)

// Unset, the admin clients are the bundled realm's portal client and the
// CLI's — whichever client the CLI is told to sign in as, so a renamed CLI
// client keeps working for admin commands. Set, the list is exactly what was
// given.
func TestAdminClients(t *testing.T) {
	for _, c := range []struct {
		name, admin, cli string
		want             []string
	}{
		{"defaults", "", "", []string{"zaentrum-web", "zae"}},
		{"a CLI client of another name", "", "acme-cli", []string{"zaentrum-web", "acme-cli"}},
		{"named explicitly", "portal-demo, zae-demo", "zae-demo", []string{"portal-demo", "zae-demo"}},
		{"named as nothing", " , ", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PORTAL_ADMIN_CLIENTS", c.admin)
			t.Setenv("PORTAL_CLI_CLIENT_ID", c.cli)
			if got := Load().AdminClients; !slices.Equal(got, c.want) {
				t.Errorf("AdminClients = %q, want %q", got, c.want)
			}
		})
	}
}

// The catalog manager the setup checklist reads is the bundled one unless
// named — and "-" names none.
func TestKatalogManagerURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                    "http://katalog-manager-api",
		"http://catalog:8080": "http://catalog:8080",
		"-":                   "",
	} {
		t.Setenv("PORTAL_KATALOG_MANAGER_URL", raw)
		if got := Load().KatalogManagerURL; got != want {
			t.Errorf("PORTAL_KATALOG_MANAGER_URL=%q: %q, want %q", raw, got, want)
		}
	}
}
