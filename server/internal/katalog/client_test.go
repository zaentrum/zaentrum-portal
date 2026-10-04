package katalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeManager answers katalog-manager's GraphQL endpoint with answer, and
// records what each request carried.
type fakeManager struct {
	*httptest.Server
	bearers []string
	queries []string
	vars    []map[string]any
}

func newFakeManager(t *testing.T, answer func(query string, vars map[string]any) (int, string)) *fakeManager {
	t.Helper()
	f := &fakeManager{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		f.queries = append(f.queries, body.Query)
		f.vars = append(f.vars, body.Variables)
		code, out := answer(body.Query, body.Variables)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(f.Close)
	return f
}

func ids(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"id":"%d"}`, i)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestOverview(t *testing.T) {
	f := newFakeManager(t, func(string, map[string]any) (int, string) {
		return 200, `{"data":{
		  "settings":[{"key":"omdb.api_key","isSecret":true,"isSet":true,"updatedAt":null},
		              {"key":"tmdb.api_key","isSecret":true,"isSet":true,"updatedAt":"2026-10-04T06:00:00Z"}],
		  "enrichStatus":{"tmdbEnabled":false},
		  "movies":` + ids(3) + `, "series":` + ids(CountLimit) + `,
		  "scanJobs":[{"id":"j1","status":"done","startedAt":"2026-10-04T06:01:00Z","finishedAt":"2026-10-04T06:02:00Z",
		               "errorMessage":null,"filesSeen":14,"itemsInserted":12,"itemsUpdated":2}]}}`
	})
	c := New(f.URL + "/")
	o, err := c.Overview(context.Background(), "Bearer admin-token")
	if err != nil {
		t.Fatal(err)
	}
	// The caller's bearer is forwarded as it came; portal-api has none of its own.
	if len(f.bearers) != 1 || f.bearers[0] != "Bearer admin-token" {
		t.Errorf("bearers = %q", f.bearers)
	}
	if o.TMDBKey == nil || !o.TMDBKey.IsSet || o.TMDBKey.UpdatedAt == nil || o.EnvironmentKey {
		t.Errorf("tmdb key = %+v, environment %v", o.TMDBKey, o.EnvironmentKey)
	}
	// Series reached the page limit: the count is a floor.
	if o.Titles() != 3+CountLimit || !o.More {
		t.Errorf("titles = %d, more = %v", o.Titles(), o.More)
	}
	if o.LastScan == nil || o.LastScan.Status != "done" || o.LastScan.FilesSeen != 14 || o.LastScan.ItemsInserted != 12 || o.LastScan.Error != "" {
		t.Errorf("last scan = %+v", o.LastScan)
	}
	for _, field := range []string{"settings", "enrichStatus", "movies(limit: 200)", "series(limit: 200)", "scanJobs(limit: 1)"} {
		if !strings.Contains(f.queries[0], field) {
			t.Errorf("the query asks for no %s: %s", field, f.queries[0])
		}
	}
}

func TestOverviewOfAnEmptyCatalog(t *testing.T) {
	f := newFakeManager(t, func(string, map[string]any) (int, string) {
		return 200, `{"data":{"settings":[],"enrichStatus":{"tmdbEnabled":true},"movies":[],"series":[],"scanJobs":[]}}`
	})
	o, err := New(f.URL).Overview(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if o.TMDBKey != nil || !o.EnvironmentKey || o.Titles() != 0 || o.More || o.LastScan != nil {
		t.Errorf("overview = %+v", o)
	}
	if f.bearers[0] != "" {
		t.Errorf("no bearer to forward, and one was sent: %q", f.bearers[0])
	}
}

// Refused, failed and unreachable are told apart: the checklist says which.
func TestErrors(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		body   string
		expect func(error) bool
	}{
		{"forbidden field", 200, `{"data":null,"errors":[{"message":"forbidden: settings requires the zaentrum-admin role","extensions":{"code":"FORBIDDEN"}}]}`,
			func(err error) bool {
				var r *Refused
				return errors.As(err, &r) && strings.Contains(r.Message, "zaentrum-admin")
			}},
		{"unauthorized", 401, `unauthorized`, func(err error) bool { var r *Refused; return errors.As(err, &r) }},
		{"another error", 200, `{"errors":[{"message":"scanner not configured"}]}`,
			func(err error) bool {
				var f *Failed
				return errors.As(err, &f) && f.Message == "scanner not configured"
			}},
		{"a server failing", 502, `bad gateway`, func(err error) bool { var f *Failed; return errors.As(err, &f) }},
		{"no GraphQL", 200, `<html>`, func(err error) bool { var f *Failed; return errors.As(err, &f) }},
	}
	for _, c := range cases {
		f := newFakeManager(t, func(string, map[string]any) (int, string) { return c.code, c.body })
		if _, err := New(f.URL).Overview(context.Background(), "Bearer x"); !c.expect(err) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	gone := newFakeManager(t, nil)
	gone.Close()
	if _, err := New(gone.URL).Overview(context.Background(), "Bearer x"); !errors.Is(err, ErrUnreachable) {
		t.Errorf("a manager that does not answer: err = %v", err)
	}
	if New("  ") != nil {
		t.Error("no address is no client")
	}
}

// The key goes to katalog-manager as a variable, and never comes back in an
// error, whatever katalog-manager's error repeats.
func TestSetSecretSetting(t *testing.T) {
	const value = "eyJhbGciOiJIUzI1NiJ9.the-tmdb-token"
	f := newFakeManager(t, func(_ string, vars map[string]any) (int, string) {
		if vars["v"] == value {
			return 200, `{"data":{"setSecretSetting":{"key":"tmdb.api_key","isSecret":true,"isSet":true,"updatedAt":"2026-10-04T06:00:00Z"}}}`
		}
		return 200, fmt.Sprintf(`{"errors":[{"message":"could not store %v for tmdb.api_key"}]}`, vars["v"])
	})
	c := New(f.URL)
	s, err := c.SetSecretSetting(context.Background(), "Bearer admin", TMDBSetting, value)
	if err != nil || !s.IsSet || s.Key != TMDBSetting {
		t.Fatalf("set = %+v, %v", s, err)
	}
	if f.vars[0]["k"] != TMDBSetting || f.vars[0]["v"] != value || strings.Contains(f.queries[0], value) {
		t.Errorf("the key travels as a variable, not in the query: %q %v", f.queries[0], f.vars[0])
	}
	_, err = c.SetSecretSetting(context.Background(), "Bearer admin", TMDBSetting, "another-secret-value")
	if err == nil || strings.Contains(err.Error(), "another-secret-value") || !strings.Contains(err.Error(), "could not store ***") {
		t.Errorf("err = %v", err)
	}
}

func TestTriggerScan(t *testing.T) {
	f := newFakeManager(t, func(query string, _ map[string]any) (int, string) {
		if !strings.Contains(query, "triggerScan") {
			return 200, `{"errors":[{"message":"unexpected"}]}`
		}
		return 200, `{"data":{"triggerScan":{"id":"j2","status":"running","startedAt":"2026-10-04T06:05:00Z","finishedAt":null,
		  "errorMessage":null,"filesSeen":0,"itemsInserted":0,"itemsUpdated":0}}}`
	})
	j, err := New(f.URL).TriggerScan(context.Background(), "Bearer admin")
	if err != nil || j.ID != "j2" || j.Status != "running" || j.StartedAt == nil || j.FinishedAt != nil {
		t.Errorf("scan = %+v, %v", j, err)
	}
}
