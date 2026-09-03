package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The aggregator's contract: keep what validates, skip what doesn't, and let
// a broken candidate cost only itself. One valid descriptor among a 404, a
// garbage body, an anonymous one and a dead host must yield exactly one entry.
func TestCollectDescriptorsKeepsOnlyValid(t *testing.T) {
	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wellKnownCapability {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"service":"sample-addon","kind":"addon","version":"1.2.3",
			"commands":[{"name":"list","summary":"list things","method":"GET","path":"/api/things"}],
			"checks":[{"name":"system","path":"/api/health/system"}],
			"topics":["sample.thing.done"]}`)
	}))
	defer valid.Close()

	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>definitely not json</html>")
	}))
	defer garbage.Close()

	// A descriptor that cannot name its service describes nothing.
	anonymous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"kind":"addon"}`)
	}))
	defer anonymous.Close()

	got := collectDescriptors(context.Background(), []string{
		valid.URL, notFound.URL, garbage.URL, anonymous.URL,
		"http://127.0.0.1:1", // dead host: must cost its timeout, not the call
	})
	if len(got) != 1 {
		t.Fatalf("want exactly the one valid descriptor, got %d: %+v", len(got), got)
	}
	d := got[0]
	if d.Service != "sample-addon" || d.Kind != "addon" || len(d.Commands) != 1 || len(d.Checks) != 1 {
		t.Fatalf("descriptor did not survive intact: %+v", d)
	}
}

// No candidates must serialize as an empty list, not null — zae decodes
// services as a slice and "null" is the kind of edge that becomes a panic in
// somebody else's client.
func TestCollectDescriptorsEmptyIsEmptySlice(t *testing.T) {
	got := collectDescriptors(context.Background(), nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty slice, got %#v", got)
	}
}
