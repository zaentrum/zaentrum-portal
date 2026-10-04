package redact

import (
	"encoding/json"
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

// A document is redacted field by field, never as its encoded text: what a
// credential-named field holds goes whole — an array or an object keeps its
// shape, each string and number in it does not — and every other string is
// scrubbed as the text it holds.
func TestDocument(t *testing.T) {
	raw := `{
	  "note": "sent token=\"s3cr3t-one\" next=field",
	  "unquoted": "token=s3cr3t-two\"tail",
	  "tokens": ["s3cr3t-three", "s3cr3t-four"],
	  "auth": {"clientSecret": {"current": "s3cr3t-five", "rotations": 3}},
	  "apiKeyCount": 1234567,
	  "passwordSet": true,
	  "secretName": "",
	  "count": 12345678901234567890,
	  "items": [{"password": "s3cr3t-six"}, "Authorization: Bearer s3cr3t-seven-0123456789"]
	}`
	doc, err := DecodeJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// What comes out is a document: it decodes again.
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("the redacted document is no JSON: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "s3cr3t") {
		t.Errorf("a secret survived: %s", out)
	}
	if got := back["note"]; got != `sent token="`+Marker+`" next=field` {
		t.Errorf("note = %q — a string is scrubbed as the text it holds", got)
	}
	if got := back["unquoted"]; got != `token=`+Marker+`"tail` {
		t.Errorf("unquoted = %q", got)
	}
	if got, ok := back["tokens"].([]any); !ok || len(got) != 2 || got[0] != Marker || got[1] != Marker {
		t.Errorf("tokens = %#v — an array under a credential keeps its shape and loses its contents", back["tokens"])
	}
	inner, _ := back["auth"].(map[string]any)["clientSecret"].(map[string]any)
	if inner["current"] != Marker || inner["rotations"] != Marker {
		t.Errorf("clientSecret = %#v — a nested object under a credential loses every value", inner)
	}
	if back["apiKeyCount"] != Marker {
		t.Errorf("apiKeyCount = %#v — a number under a credential goes too", back["apiKeyCount"])
	}
	// A boolean, a null and an empty string say nothing a credential is.
	if back["passwordSet"] != true || back["secretName"] != "" {
		t.Errorf("passwordSet = %#v, secretName = %#v", back["passwordSet"], back["secretName"])
	}
	// Numbers keep the digits they were written with.
	if !strings.Contains(string(out), `"count":12345678901234567890`) {
		t.Errorf("a number was rewritten: %s", out)
	}
	if _, err := DecodeJSON([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Error("two documents decode as one")
	}
}

// A field names a credential by the rule a key of key=value text does.
func TestSecretField(t *testing.T) {
	for _, k := range []string{"password", "DB_PASSWORD", "clientSecret", "client-secret", "api_key", "apikey", "access-key", "private_key", "tokens", "credential.id"} {
		if !SecretField(k) {
			t.Errorf("%q should name a credential", k)
		}
	}
	// A pod/container log key is no field name, whatever it spells.
	for _, k := range []string{"title", "itemId", "secretary/app", "token-refresher-7d9f/app", "count", ""} {
		if SecretField(k) {
			t.Errorf("%q should not name a credential", k)
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
