package k8s

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A write to a Secret is answered with the Secret — values included. The
// client must hand none of that back, and must say precisely what it sent.
func TestSecretWritesAreWriteOnly(t *testing.T) {
	type call struct{ method, path, contentType, auth, body string }
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(b)})
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"NotFound","message":"secrets \"missing\" not found","code":404}`))
			return
		}
		_, _ = w.Write([]byte(`{"kind":"Secret","data":{"config.password":"c2VjcmV0"}}`))
	}))
	defer srv.Close()
	c := NewAt(srv.URL, "zaentrum", "token-1", srv.Client())
	ctx := context.Background()

	if err := c.CreateSecret(ctx, []byte(`{"metadata":{"name":"s"}}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.PatchSecret(ctx, "s", []byte(`{"data":{"a":null}}`)); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if err := c.DeleteSecret(ctx, "s"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err := c.DeleteSecret(ctx, "missing")
	if !IsNotFound(err) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a Status answer must surface as a typed error, got %v", err)
	}

	want := []call{
		{http.MethodPost, "/api/v1/namespaces/zaentrum/secrets", "application/json", "Bearer token-1", `{"metadata":{"name":"s"}}`},
		{http.MethodPatch, "/api/v1/namespaces/zaentrum/secrets/s", "application/merge-patch+json", "Bearer token-1", `{"data":{"a":null}}`},
		{http.MethodDelete, "/api/v1/namespaces/zaentrum/secrets/s", "", "Bearer token-1", ""},
		{http.MethodDelete, "/api/v1/namespaces/zaentrum/secrets/missing", "", "Bearer token-1", ""},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

func TestResourceCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrumaddons/example":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"Conflict","message":"the object has been modified","code":409}`))
		case r.Method == http.MethodGet && r.URL.Path == "/apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrumaddons/example":
			_, _ = w.Write([]byte(`{"metadata":{"name":"example"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewAt(srv.URL+"/", "ns", "", nil)
	ctx := context.Background()

	raw, err := c.GetResource(ctx, "zaentrum.io", "v1alpha1", "zaentrumaddons", "example")
	if err != nil || !strings.Contains(string(raw), `"example"`) {
		t.Fatalf("get = %s, %v", raw, err)
	}
	if _, err := c.UpdateResource(ctx, "zaentrum.io", "v1alpha1", "zaentrumaddons", "example", []byte(`{}`)); !IsConflict(err) {
		t.Errorf("a stale update must read as a conflict, got %v", err)
	}
	// A resource type the cluster does not serve answers a plain 404 page, not
	// a Status: still NotFound.
	if _, err := c.GetResource(ctx, "zaentrum.io", "v1alpha1", "unknown", "x"); !IsNotFound(err) {
		t.Errorf("unknown type = %v, want NotFound", err)
	}
}
