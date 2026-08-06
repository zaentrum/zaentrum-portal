package dbbrowse

import "testing"

// The curated list names databases logically. Using those names as physical
// database names meant the BETA admin console resolved "katalog" to
// PRODUCTION's catalog database — a live cross-environment read, stopped only
// by a missing grant.
func TestEnvSuffix(t *testing.T) {
	cases := []struct {
		name, dsn, want string
	}{
		{
			"beta",
			"postgres://u:p@postgres.example.com:5432/portal_beta?sslmode=require",
			"_beta",
		},
		{
			"production is unsuffixed",
			"postgres://u:p@postgres.example.com:5432/portal?sslmode=require",
			"",
		},
		{
			"demo",
			"postgres://u:p@host:5432/portal_demo",
			"_demo",
		},
		// Fail closed. Returning "" here would resolve every other curated
		// table to the unsuffixed — i.e. production — database, which is the
		// bug being fixed.
		{
			"an unrecognised portal database refuses to guess",
			"postgres://u:p@host:5432/something_else",
			unknownSuffix,
		},
		{"an unparseable dsn yields no suffix", "not a dsn", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := envSuffix(c.dsn); got != c.want {
				t.Fatalf("envSuffix(%q) = %q, want %q", c.dsn, got, c.want)
			}
		})
	}
}

func TestPhysical(t *testing.T) {
	b := &Browser{suffix: "_beta"}
	for logical, want := range map[string]string{
		"katalog": "katalog_beta",
		"portal":  "portal_beta",
	} {
		if got := b.physical(logical); got != want {
			t.Fatalf("physical(%q) = %q, want %q", logical, got, want)
		}
	}
	// Production maps logical to itself.
	prod := &Browser{suffix: ""}
	if got := prod.physical("katalog"); got != "katalog" {
		t.Fatalf("production physical(katalog) = %q, want katalog", got)
	}
}
