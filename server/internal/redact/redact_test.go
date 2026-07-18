package redact

import (
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		mustRedact []string // substrings that must NOT survive
		keep       []string // substrings that must survive
	}{
		{
			name:       "basic auth header (base64 decodes to user:pass)",
			in:         `Authorization: Basic cG9ydGFsOnMzY3JldA==`,
			mustRedact: []string{"cG9ydGFsOnMzY3JldA=="},
			keep:       []string{"Basic"},
		},
		{
			name:       "bare basic base64",
			in:         `curl -H 'basic cG9ydGFsOnMzY3JldA==' http://x`,
			mustRedact: []string{"cG9ydGFsOnMzY3JldA=="},
		},
		{
			name:       "bearer via authorization header",
			in:         `Authorization: Bearer sk-9f8a7b6c5d4e3f2a1b0c9d8e`,
			mustRedact: []string{"sk-9f8a7b6c5d4e3f2a1b0c9d8e"},
			keep:       []string{"Bearer"},
		},
		{
			name: "bearer prose is NOT over-redacted",
			in:   `oidc discovery succeeded; bearer verification active`,
			keep: []string{"bearer verification active"},
		},
		{
			name:       "postgres dsn with @ in the password",
			in:         `dsn postgres://portal:p@ssw0rd@host:5432/db loaded`,
			mustRedact: []string{"ssw0rd"},
			keep:       []string{"postgres://portal:", "@host:5432/db", "loaded"},
		},
		{
			name:       "redis dsn with empty username",
			in:         `REDIS_URL=redis://:mypassw0rd@valkey:6379/0`,
			mustRedact: []string{"mypassw0rd"},
			keep:       []string{"redis://:", "@valkey:6379/0"},
		},
		{
			name:       "key=value secret containing commas",
			in:         `password=aaa,bbb,ccc next=field`,
			mustRedact: []string{"aaa,bbb,ccc", "bbb", "ccc"},
			keep:       []string{"password", "next=field"},
		},
		{
			name:       "standard postgres dsn still redacted",
			in:         `postgres://katalog:s3cr3tpw@postgres:5432/katalog`,
			mustRedact: []string{"s3cr3tpw"},
			keep:       []string{"postgres://katalog:", "@postgres:5432/katalog"},
		},
		{
			name: "ordinary log line untouched",
			in:   `keyframe.uploaded item=49131b58 kind=backdrop bytes=176656`,
			keep: []string{"keyframe.uploaded", "49131b58", "176656"},
		},
	}
	for _, c := range cases {
		got := Secrets(c.in)
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

func TestLooksSecretKey(t *testing.T) {
	for _, k := range []string{"password", "DB_PASSWORD", "clientSecret", "api_key", "accessToken", "private_key"} {
		if !LooksSecretKey(k) {
			t.Errorf("%q should look secret", k)
		}
	}
	for _, k := range []string{"id", "title", "item_id", "createdat", "email"} {
		if LooksSecretKey(k) {
			t.Errorf("%q should NOT look secret", k)
		}
	}
}
