package config

import (
	"slices"
	"testing"
	"time"
)

// A notice is kept 90 days unless PORTAL_NOTICE_RETENTION names another
// positive duration; one that is none keeps the default.
func TestNoticeRetention(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      90 * 24 * time.Hour,
		"720h":  30 * 24 * time.Hour,
		"0s":    90 * 24 * time.Hour,
		"-1h":   90 * 24 * time.Hour,
		"three": 90 * 24 * time.Hour,
	} {
		t.Setenv("PORTAL_NOTICE_RETENTION", raw)
		if got := Load().NoticeRetention; got != want {
			t.Errorf("PORTAL_NOTICE_RETENTION=%q: %s, want %s", raw, got, want)
		}
	}
}

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
