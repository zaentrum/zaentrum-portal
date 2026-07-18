package operator

import (
	"strings"
	"testing"
)

func TestScrubSecrets(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		mustRedact []string // substrings that must NOT survive
		keep       []string // substrings that must survive
	}{
		{
			name:       "password key=value",
			in:         `DB_PASSWORD=hunter2 starting up`,
			mustRedact: []string{"hunter2"},
			keep:       []string{"DB_PASSWORD", "starting up"},
		},
		{
			name:       "client secret json",
			in:         `{"client_secret":"abc123XYZ","user":"demo"}`,
			mustRedact: []string{"abc123XYZ"},
			keep:       []string{"client_secret", "demo"},
		},
		{
			name:       "bearer token",
			in:         `Authorization: Bearer sk-9f8a7b6c5d4e3f2a1b0c9d8e`,
			mustRedact: []string{"sk-9f8a7b6c5d4e3f2a1b0c9d8e"},
			keep:       []string{"Bearer"},
		},
		{
			name:       "jwt",
			in:         `token=eyJhbGciOiJIUzI1Ni19.eyJzdWIiOiIxMjM0NTY.SflKxwRJSMeKKF2QT4`,
			mustRedact: []string{"eyJhbGciOiJIUzI1Ni19", "SflKxwRJSMeKKF2QT4"},
		},
		{
			name:       "postgres url creds",
			in:         `dsn postgres://katalog:s3cr3tpw@postgres:5432/katalog`,
			mustRedact: []string{"s3cr3tpw"},
			keep:       []string{"postgres://katalog:", "@postgres:5432/katalog"},
		},
		{
			name: "ordinary log line untouched",
			in:   `2026-07-18T14:13 keyframe.uploaded item=49131b58 kind=backdrop bytes=176656`,
			keep: []string{"keyframe.uploaded", "49131b58", "176656"},
		},
	}
	for _, c := range cases {
		got := ScrubSecrets(c.in)
		for _, r := range c.mustRedact {
			if strings.Contains(got, r) {
				t.Errorf("%s: secret %q survived: %q", c.name, r, got)
			}
		}
		for _, k := range c.keep {
			if !strings.Contains(got, k) {
				t.Errorf("%s: expected %q to survive, got %q", c.name, k, got)
			}
		}
	}
}
