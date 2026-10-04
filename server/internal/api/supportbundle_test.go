package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/redact"
)

// The support bundle is a document an admin hands to someone else, so its
// last redaction must leave it a document. Over its encoded text the text
// rule took the escaped quote of `token=abc\"…` for the end of the value: the
// JSON broke, and what followed the quote stayed.
func TestSupportBundleIsADocumentWithoutItsSecrets(t *testing.T) {
	e := newTokenEnv(t)
	e.store.apps["notes"] = model.App{Key: "notes", Title: "notes", Enabled: true,
		Description: `token=s3cr3t-abc"… and a header Authorization: Bearer s3cr3t-0123456789abcdef`}
	e.store.apps["wiki"] = model.App{Key: "wiki", Title: "wiki", Enabled: true,
		Description: `reached with password="s3cr3t-quoted" since monday`}

	rec := e.do(adminPortal, http.MethodGet, "/api/portal/debug/support-bundle?logs=0&kafka=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle = %d %s", rec.Code, rec.Body)
	}
	var bundle struct {
		Kind     string `json:"kind"`
		Sections struct {
			Registry struct {
				Apps []model.App `json:"apps"`
			} `json:"registry"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("the bundle is no JSON: %v\n%s", err, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Errorf("a secret survived the bundle:\n%s", rec.Body)
	}
	if bundle.Kind != "zaentrum-support-bundle" || len(bundle.Sections.Registry.Apps) != 2 {
		t.Fatalf("bundle = %+v", bundle)
	}
	want := map[string]string{
		"notes": `token=` + redact.Marker + `"… and a header Authorization: Bearer ` + redact.Marker,
		"wiki":  `reached with password="` + redact.Marker + `" since monday`,
	}
	for _, app := range bundle.Sections.Registry.Apps {
		if app.Description != want[app.Key] {
			t.Errorf("%s: description = %q, want %q", app.Key, app.Description, want[app.Key])
		}
	}
}

// Under a credential-named field the whole value goes, whatever its shape:
// an array keeps its elements' places and loses their contents, a number
// becomes the marker, and the document stays one.
func TestScrubBundleRedactsWhatACredentialFieldHolds(t *testing.T) {
	raw, err := scrubBundle(map[string]any{
		"kind": "zaentrum-support-bundle",
		"sections": map[string]any{
			"config": map[string]any{
				"apiTokens":      []string{"s3cr3t-one", "s3cr3t-two"},
				"clientSecrets":  map[string]any{"cli": "s3cr3t-three"},
				"tokenLifetime":  300,
				"tokenRequired":  true,
				"adminRole":      "zaentrum-admin",
				"protectedNames": []string{"postgres", "kafka"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sections struct {
			Config map[string]any `json:"config"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the bundle is no JSON: %v\n%s", err, raw)
	}
	if strings.Contains(string(raw), "s3cr3t") {
		t.Errorf("a secret survived:\n%s", raw)
	}
	cfg := doc.Sections.Config
	if tokens, ok := cfg["apiTokens"].([]any); !ok || len(tokens) != 2 || tokens[0] != redact.Marker {
		t.Errorf("apiTokens = %#v", cfg["apiTokens"])
	}
	if cfg["tokenLifetime"] != redact.Marker || cfg["tokenRequired"] != true {
		t.Errorf("tokenLifetime = %#v, tokenRequired = %#v", cfg["tokenLifetime"], cfg["tokenRequired"])
	}
	// What no credential names stays as it was.
	if cfg["adminRole"] != "zaentrum-admin" || len(cfg["protectedNames"].([]any)) != 2 {
		t.Errorf("config = %#v", cfg)
	}
	if !strings.Contains(string(raw), "\n  \"kind\"") {
		t.Errorf("the bundle is no longer indented:\n%s", raw)
	}
}
