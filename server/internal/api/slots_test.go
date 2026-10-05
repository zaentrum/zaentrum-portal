package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

const own = "https://media.example.org"

// slotURLsTaken and slotURLsRefused are the cases of the slot URL rule,
// against the origin own — each refusal with what its error mentions. A
// notice's link is held to the same cases (notices_test.go).
var (
	slotURLsTaken = []string{
		"/portal/app/example?q={q}",
		"/portal/app/example?q={q}#/discover",
		"/api/portal/apps/example/api/request?title={q}",
		"/",
		"https://media.example.org/portal/app/example?q={q}",
		"HTTPS://Media.Example.org/portal/app/example",
		"https://media.example.org:443/portal/app/example",
		"https://media.example.org",
		"  /portal/app/example  ",
		"/portal/app/example?next=https://elsewhere.example", // a query is the destination's business
		"/v%2e1/items", // an encoded dot inside a segment climbs nowhere
	}
	slotURLsRefused = []struct{ url, mention string }{
		{"", "required"},
		{"javascript:alert(document.cookie)", "never to a javascript: URL"},
		{"JavaScript:alert(1)", "never to a javascript: URL"},
		{"java\tscript:alert(1)", "control characters"},
		{"data:text/html,<script>alert(1)</script>", "never to a data: URL"},
		{"data:text/html;base64,PHNjcmlwdD4=", "never to a data: URL"},
		{"file:///etc/passwd", "never to a file: URL"},
		{"vbscript:msgbox", "never to a vbscript: URL"},
		{"https://elsewhere.example/steal?t={q}", "not to this instance"},
		{"https://media.example.org.elsewhere.example/x", "not to this instance"},
		{"http://media.example.org/portal/app/example", "not to this instance"}, // another scheme is another origin
		{"https://media.example.org:8443/x", "not to this instance"},
		{"https://user:pass@media.example.org/x", "credentials"},
		{"https://media.example.org@elsewhere.example/x", "credentials"},
		{"//elsewhere.example/x", "protocol-relative"},
		{"/\\elsewhere.example/x", "backslashes"},
		{"/\n/elsewhere.example", "control characters"},
		{"https:elsewhere.example", "no host"},
		{"https:/elsewhere.example", "no host"},
		{"portal/app/example", "must be a path on this instance"},
		{"?q=x", "must be a path on this instance"},
		{"#/x", "must be a path on this instance"},
		{"/api/portal/apps/example/../../addons", "'..'"},
		{"/api/portal/apps/example/%2e%2e/%2e%2e/addons", "'..'"},
		{"/api/portal/apps/example/./x", "'.'"},
		{"https://media.example.org/api/portal/apps/x/../../addons", "'..'"},
		{"/api/portal/apps/example%2f..%2faddons", "encoded '/'"},
	}
)

// A slot URL is rendered by a product app as a link, or fetched with the
// signed-in user's bearer. It may lead to a path on this instance or to its
// own origin, over http(s) — nowhere else.
func TestSlotURLRules(t *testing.T) {
	origins := []string{own}
	for _, ok := range slotURLsTaken {
		if _, err := slotURL(ok, origins); err != nil {
			t.Errorf("slotURL(%q) = %v, want accepted", ok, err)
		}
	}
	for _, c := range slotURLsRefused {
		_, err := slotURL(c.url, origins)
		if err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("slotURL(%q) = %v, want refused mentioning %q", c.url, err, c.mention)
		}
	}
	// An instance that does not know its origin accepts paths only.
	if _, err := slotURL("https://media.example.org/x", nil); err == nil || !strings.Contains(err.Error(), "knows no origin") {
		t.Errorf("absolute without origins = %v", err)
	}
	if _, err := slotURL("/portal/app/example", nil); err != nil {
		t.Errorf("a path needs no origin: %v", err)
	}
}

func TestCheckSlotKindMethodAndActions(t *testing.T) {
	row := func(kind, method, url string) model.Extension {
		return model.Extension{Key: "example.x", Addon: "example", Slot: "search.empty", Kind: kind, Method: method, URL: url}
	}
	origins := []string{own}
	// Defaults: a link, sent nowhere, stored as POST as before.
	got, err := checkSlot(row("", "", "/portal/app/example"), origins, "")
	if err != nil || got.Kind != "link" || got.Method != "POST" {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	if got, err := checkSlot(row("action", "post", "/api/portal/apps/example/api/x"), origins, proxyPath("example")); err != nil || got.Method != "POST" {
		t.Errorf("an action to the addon's own API = %+v, %v", got, err)
	}
	for _, c := range []struct {
		name     string
		row      model.Extension
		prefix   string
		mentions string
	}{
		{"unknown kind", row("button", "", "/x"), "", "link or action"},
		{"GET", row("action", "GET", "/api/portal/apps/example/x"), "", "POST"},
		{"DELETE", row("action", "DELETE", "/api/portal/apps/example/x"), "", "POST"},
		{"a link with a method", row("link", "PUT", "/x"), "", "POST"},
		{"an addon's action into another addon", row("action", "", "/api/portal/apps/other/api/x"), proxyPath("example"), "its own API"},
		{"an addon's action into the media API", row("action", "", "/api/v1/items/1/delete"), proxyPath("example"), "its own API"},
		{"an addon's action into the registry", row("action", "", "https://media.example.org/api/portal/addons"), proxyPath("example"), "its own API"},
		{"an addon's action named by prefix only", row("action", "", "/api/portal/apps/example-evil/x"), proxyPath("example"), "its own API"},
		{"a bad status url", model.Extension{Kind: "link", URL: "/x", StatusURL: "javascript:alert(1)"}, "", "status url"},
	} {
		_, err := checkSlot(c.row, origins, c.prefix)
		if err == nil || !strings.Contains(err.Error(), c.mentions) {
			t.Errorf("%s: %v, want refused mentioning %q", c.name, err, c.mentions)
		}
	}
	// An addon's link may lead anywhere on the instance: it carries no token.
	if _, err := checkSlot(row("link", "", "/portal/app/other"), origins, proxyPath("example")); err != nil {
		t.Errorf("a link elsewhere on the instance: %v", err)
	}
}

// A manifest's slot rows obey the rules, and one that breaks them refuses the
// install: dropping it would hide from the admin why the button is missing.
func TestPlanAddonSlotRules(t *testing.T) {
	plan := func(slot string) (addonPlan, error) {
		return planAddon("http://example", decodeManifest(t, `{"service":"example","ui":{"slots":[`+slot+`]}}`), "apps", own)
	}
	for name, slot := range map[string]string{
		"javascript":             `{"slot":"search.empty","label":"x","url":"javascript:alert(1)"}`,
		"another host":           `{"slot":"search.empty","label":"x","url":"https://elsewhere.example/x"}`,
		"protocol-relative":      `{"slot":"search.empty","label":"x","url":"//elsewhere.example/x"}`,
		"unknown kind":           `{"slot":"search.empty","label":"x","kind":"script","url":"/x"}`,
		"a method":               `{"slot":"search.empty","label":"x","kind":"action","method":"DELETE","url":"/api/portal/apps/example/x"}`,
		"an action elsewhere":    `{"slot":"search.empty","label":"x","kind":"action","url":"/api/v1/items/{id}"}`,
		"a climbing action":      `{"slot":"search.empty","label":"x","kind":"action","url":"/api/portal/apps/example/../../addons"}`,
		"a path that is not one": `{"slot":"search.empty","label":"x","url":"portal/app/example"}`,
	} {
		_, err := plan(slot)
		var me *manifestError
		if !errors.As(err, &me) || !strings.Contains(err.Error(), "ui.slots[0]") {
			t.Errorf("%s: want a manifest error naming ui.slots[0], got %v", name, err)
		}
	}
	p, err := plan(`{"slot":"search.empty","label":"x","kind":"action","url":"/api/portal/apps/example/api/request?q={q}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Rows[0]; r.URL != own+"/api/portal/apps/example/api/request?q={q}" || r.Method != "POST" || r.Kind != "action" {
		t.Errorf("row = %+v — a path is made absolute against the origin, the method is POST", r)
	}
	// A row with no url is a contribution with nowhere to go: dropped, as one
	// without a slot or a label is.
	if p, err := plan(`{"slot":"search.empty","label":"x"}`); err != nil || len(p.Rows) != 0 {
		t.Errorf("a row without a url = %+v, %v", p.Rows, err)
	}
	// The origin itself must be one.
	if _, err := planAddon("http://example", decodeManifest(t, `{"service":"example","ui":{"slots":[{"slot":"s","label":"x","url":"/x"}]}}`), "apps", "javascript:alert(1)"); err == nil {
		t.Error("a public origin that is no origin must refuse the plan")
	}
}

// publicBase is the origin a scripted install makes slot URLs absolute with.
// It is an origin, and — when the platform names its hostname — that one.
func TestInstallAddonPublicBase(t *testing.T) {
	addr := manifestServer(t, `{"service":"example","components":[{"name":"example","workload":"localhost","role":"primary"}],"ui":{"slots":[{"slot":"search.empty","label":"x","url":"/portal/app/example"}]}}`)
	for _, c := range []struct {
		base string
		want int
	}{
		{"https://media.example.org", http.StatusOK},
		{"https://media.example.org/", http.StatusOK},
		{"javascript:alert(1)", http.StatusBadRequest},
		{"https://media.example.org/portal", http.StatusBadRequest},
		{"https://user@media.example.org", http.StatusBadRequest},
	} {
		fake := newFakeStore()
		rec := postInstall(t, &API{addons: fake}, map[string]any{"proxyUrl": addr, "publicBase": c.base})
		if rec.Code != c.want {
			t.Errorf("publicBase %q = %d %s, want %d", c.base, rec.Code, rec.Body, c.want)
		}
		if c.want == http.StatusOK && (len(fake.installs) != 1 || fake.installs[0].Rows[0].URL != own+"/portal/app/example") {
			t.Errorf("publicBase %q: rows = %+v", c.base, fake.installs)
		}
	}
	// The platform names its hostname: publicBase must be it.
	e := newChartEnv(t)
	e.kube.Put("zaentrums", map[string]any{"metadata": map[string]any{"name": "zaentrum"}, "spec": map[string]any{"hostname": "media.example.org"}})
	if rec := postInstall(t, e.api, map[string]any{"proxyUrl": addr, "publicBase": "https://elsewhere.example"}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not this platform's origin") {
		t.Errorf("publicBase on another host = %d %s", rec.Code, rec.Body)
	}
}

// The write API keeps the same rules, and refuses before it stores anything.
func TestExtensionWritesObeyTheSlotRules(t *testing.T) {
	e := newTokenEnv(t)
	post := func(row map[string]any) int {
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/extensions", row)
		return rec.Code
	}
	row := func(kv ...any) map[string]any {
		m := map[string]any{"key": "example.hint", "addon": "example", "slot": "search.empty", "label": "x", "url": "/portal/app/example?q={q}"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for name, body := range map[string]map[string]any{
		"javascript":        row("url", "javascript:alert(1)"),
		"another host":      row("url", "https://elsewhere.example/x"),
		"protocol-relative": row("url", "//elsewhere.example/x"),
		"an unknown kind":   row("kind", "script"),
		"an action as GET":  row("kind", "action", "method", "GET", "url", "/api/portal/apps/example/x"),
		"a bad status url":  row("statusUrl", "data:text/plain,x"),
		"no url":            row("url", ""),
	} {
		if code := post(body); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if len(e.store.extensions) != 0 {
		t.Fatalf("refused writes stored %v", e.store.extensions)
	}
	if code := post(row("kind", "action", "url", "/api/portal/apps/example/api/x")); code != http.StatusOK {
		t.Fatalf("a valid action = %d", code)
	}
	if got := e.store.extensions["example.hint"]; got.Method != "POST" || got.Kind != "action" {
		t.Errorf("stored = %+v", got)
	}
	// The admin's browser is on the instance: its origin is the instance's.
	req := map[string]any{"key": "example.abs", "slot": "search.empty", "label": "x", "url": "http://example.com/portal/app/example"}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/extensions", req); rec.Code != http.StatusOK {
		t.Errorf("an absolute URL on the request's own origin = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(adminPortal, http.MethodPatch, "/api/portal/extensions/example.abs", map[string]any{"slot": "search.empty", "url": "https://elsewhere.example/"}); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH to another host = %d %s", rec.Code, rec.Body)
	}
}

// A row written before the rules that breaks them is not served to product
// apps; a bad status URL is dropped from a row that is otherwise fine.
func TestSlotReadServesOnlyRowsThatObeyTheRules(t *testing.T) {
	e := newTokenEnv(t)
	for _, x := range []model.Extension{
		{Key: "a.ok", Slot: "search.empty", Kind: "link", URL: "/portal/app/a?q={q}", Method: "POST", Enabled: true, StatusURL: "javascript:x"},
		{Key: "b.js", Slot: "search.empty", Kind: "link", URL: "javascript:alert(document.cookie)", Method: "POST", Enabled: true},
		{Key: "c.delete", Slot: "search.empty", Kind: "action", URL: "/api/portal/apps/c/x", Method: "DELETE", Enabled: true},
		{Key: "d.kind", Slot: "search.empty", Kind: "script", URL: "/x", Method: "POST", Enabled: true},
		{Key: "e.protocol-relative", Slot: "search.empty", Kind: "link", URL: "//elsewhere.example", Method: "POST", Enabled: true},
		{Key: "f.empty-method", Slot: "search.empty", Kind: "action", URL: "/api/portal/apps/f/x", Enabled: true},
	} {
		e.store.extensions[x.Key] = x
	}
	rec := e.do(viewerMedia, http.MethodGet, "/api/portal/slots/search.empty", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("slots = %d %s", rec.Code, rec.Body)
	}
	var got []model.Extension
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, x := range got {
		keys = append(keys, x.Key)
	}
	if strings.Join(keys, ",") != "a.ok,f.empty-method" {
		t.Fatalf("served %v", keys)
	}
	if got[0].StatusURL != "" || got[1].Method != "POST" {
		t.Errorf("served rows = %+v", got)
	}
}
