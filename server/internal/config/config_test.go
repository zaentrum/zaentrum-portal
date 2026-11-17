package config

import (
	"os"
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

// R.C2: the mail server, the sender and the sign-in come from the
// environment the operator gives portal-api, as it gives them: the port
// always, the further addresses comma-separated and possibly none, the user
// from the resource and the password from the Secret's optional key — absent
// or empty, there is no sign-in — with the name of that Secret, and, while
// mail is not set up, none of them.
func TestRC2MailFromTheEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"PORTAL_MAIL_HOST": "smtp.example.org", "PORTAL_MAIL_PORT": "2525", "PORTAL_MAIL_ENCRYPTION": "tls",
		"PORTAL_MAIL_FROM": "zaentrum@example.org", "PORTAL_MAIL_FROM_NAME": "Our server",
		"PORTAL_MAIL_ALERTS_TO": "ops@example.org,admin@example.org",
		"PORTAL_MAIL_USERNAME":  "zaentrum@example.org", "PORTAL_MAIL_PASSWORD": " password-for-tests ",
		"PORTAL_MAIL_SECRET": "zaentrum-mail-x7k2p", "PORTAL_PUBLIC_URL": "https://zaentrum.example.org",
	} {
		t.Setenv(k, v)
	}
	c := Load()
	m := c.Mail
	if m.Host != "smtp.example.org" || m.Port != 2525 || m.Encryption != "tls" || m.From != "zaentrum@example.org" ||
		m.FromName != "Our server" || m.Username != "zaentrum@example.org" || m.Password != " password-for-tests " {
		t.Errorf("mail = %+v", m)
	}
	if !slices.Equal(m.AlertsTo, []string{"ops@example.org", "admin@example.org"}) {
		t.Errorf("alertsTo = %q", m.AlertsTo)
	}
	if c.PublicURL != "https://zaentrum.example.org" || c.MailSecret != "zaentrum-mail-x7k2p" || !m.SetUp() {
		t.Errorf("public URL %q, secret %q, set up %v", c.PublicURL, c.MailSecret, m.SetUp())
	}
	t.Setenv("PORTAL_MAIL_ALERTS_TO", "")
	if got := Load().Mail.AlertsTo; len(got) != 0 {
		t.Errorf("no further addresses: %q", got)
	}
	// The Secret's keys are optional: empty here, absent below.
	t.Setenv("PORTAL_MAIL_USERNAME", "")
	t.Setenv("PORTAL_MAIL_PASSWORD", "")
	if got := Load().Mail; got.Username != "" || got.Password != "" {
		t.Errorf("an empty sign-in: %+v", got)
	}
	// A port that is none reads as the encryption's default.
	for _, raw := range []string{"", "0", "65536", "smtp"} {
		t.Setenv("PORTAL_MAIL_PORT", raw)
		if got := Load().Mail.Effective().Port; got != 465 {
			t.Errorf("PORTAL_MAIL_PORT=%q: port %d, want the default of tls, 465", raw, got)
		}
	}
	for _, k := range []string{"PORTAL_MAIL_HOST", "PORTAL_MAIL_PORT", "PORTAL_MAIL_ENCRYPTION", "PORTAL_MAIL_FROM",
		"PORTAL_MAIL_FROM_NAME", "PORTAL_MAIL_ALERTS_TO", "PORTAL_MAIL_USERNAME", "PORTAL_MAIL_PASSWORD", "PORTAL_MAIL_SECRET"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if c := Load(); c.Mail.SetUp() || c.Mail.Username != "" || len(c.Mail.AlertsTo) != 0 || c.MailSecret != "" {
		t.Errorf("mail not set up: %+v, secret %q", c.Mail, c.MailSecret)
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
