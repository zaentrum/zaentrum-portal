package k8s

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Creating a Secret is answered with the Secret — values included, when the
// apiserver does not honour the metadata-only answer asked for. The client
// hands back the generated name and nothing else.
func TestCreateSecretTakesOnlyTheName(t *testing.T) {
	type call struct{ method, path, contentType, accept, auth, body string }
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Accept"), r.Header.Get("Authorization"), string(b)})
		if strings.Contains(string(b), `"refused"`) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"Forbidden","message":"secrets is forbidden","code":403}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"Secret","metadata":{"name":"zaentrum-addon-example-values-x7k2p"},"data":{"config.password":"c2VjcmV0"}}`))
	}))
	defer srv.Close()
	c := NewAt(srv.URL, "zaentrum", "token-1", srv.Client())
	ctx := context.Background()

	name, err := c.CreateSecret(ctx, []byte(`{"metadata":{"generateName":"zaentrum-addon-example-values-"}}`))
	if err != nil || name != "zaentrum-addon-example-values-x7k2p" {
		t.Fatalf("create = %q, %v", name, err)
	}
	if _, err := c.CreateSecret(ctx, []byte(`{"refused":true}`)); !IsForbidden(err) || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("a Status answer must surface as a typed error, got %v", err)
	}
	want := call{http.MethodPost, "/api/v1/namespaces/zaentrum/secrets", "application/json", partialMetadata, "Bearer token-1",
		`{"metadata":{"generateName":"zaentrum-addon-example-values-"}}`}
	if len(calls) != 2 || calls[0] != want {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestResourceCalls(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.Method+" "+r.URL.RawQuery)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrumaddons/example":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"Conflict","message":"the object has been modified","code":409}`))
		case r.Method == http.MethodPost && r.URL.Path == "/apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrumaddons":
			_, _ = w.Write([]byte(`{"metadata":{"name":"example"}}`))
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
	if _, err := c.UpdateResource(ctx, "zaentrum.io", "v1alpha1", "zaentrumaddons", "example", []byte(`{}`), false); !IsConflict(err) {
		t.Errorf("a stale update must read as a conflict, got %v", err)
	}
	if _, err := c.CreateResource(ctx, "zaentrum.io", "v1alpha1", "zaentrumaddons", []byte(`{}`), true); err != nil {
		t.Errorf("dry-run create = %v", err)
	}
	if _, err := c.UpdateResource(ctx, "zaentrum.io", "v1alpha1", "zaentrumaddons", "example", []byte(`{}`), true); !IsConflict(err) {
		t.Errorf("dry-run update = %v", err)
	}
	if strings.Join(queries[1:], ",") != "PUT ,POST dryRun=All,PUT dryRun=All" {
		t.Errorf("queries = %v — only a dry run carries dryRun=All", queries)
	}
	// A resource type the cluster does not serve answers a plain 404 page, not
	// a Status: still NotFound.
	if _, err := c.GetResource(ctx, "zaentrum.io", "v1alpha1", "unknown", "x"); !IsNotFound(err) {
		t.Errorf("unknown type = %v, want NotFound", err)
	}
}
