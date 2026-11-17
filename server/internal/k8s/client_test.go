package k8s

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

// R.C2: the mail page's write is checked in a dry run before its password is
// stored — the same merge patch, with dryRun=All — and only then applied.
func TestRC2PatchResourceDryRun(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+r.Header.Get("Content-Type")+" "+string(b))
		_, _ = w.Write([]byte(`{"metadata":{"name":"zaentrum","generation":3}}`))
	}))
	defer srv.Close()
	c := NewAt(srv.URL, "ns", "", srv.Client())
	patch := []byte(`{"spec":{"mail":null}}`)
	if _, err := c.PatchResourceDryRun(context.Background(), "zaentrum.io", "v1alpha1", "zaentrums", "zaentrum", patch); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PatchResource(context.Background(), "zaentrum.io", "v1alpha1", "zaentrums", "zaentrum", patch); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`PATCH /apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrums/zaentrum?dryRun=All application/merge-patch+json {"spec":{"mail":null}}`,
		`PATCH /apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrums/zaentrum? application/merge-patch+json {"spec":{"mail":null}}`,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
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

// R.C2: the mail page's write is validated strictly — fieldValidation=Strict,
// in the dry run and in the write — so that a field the resource's schema
// does not know is refused, not dropped; the refusal names the fields, in the
// apiserver's words as Kubernetes 1.37 answers them, and no other refusal does.
func TestRC2PatchResourceStrict(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+r.Header.Get("Content-Type")+" "+string(b))
		_, _ = w.Write([]byte(`{"metadata":{"name":"zaentrum","generation":3}}`))
	}))
	defer srv.Close()
	c := NewAt(srv.URL, "ns", "", srv.Client())
	patch := []byte(`{"spec":{"mail":{"host":"smtp.example.org"}}}`)
	for _, dry := range []bool{true, false} {
		if _, err := c.PatchResourceStrict(context.Background(), "zaentrum.io", "v1alpha1", "zaentrums", "zaentrum", patch, dry); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`PATCH /apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrums/zaentrum?dryRun=All&fieldValidation=Strict application/merge-patch+json {"spec":{"mail":{"host":"smtp.example.org"}}}`,
		`PATCH /apis/zaentrum.io/v1alpha1/namespaces/ns/zaentrums/zaentrum?fieldValidation=Strict application/merge-patch+json {"spec":{"mail":{"host":"smtp.example.org"}}}`,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}

	// The apiserver's refusal, as kube-apiserver 1.37 words it: the object it
	// quotes, its own quotes escaped, then the fields.
	quoted := `{\"apiVersion\":\"zaentrum.io/v1alpha1\",\"kind\":\"Zaentrum\",\"spec\":{\"mail\":{\"host\":\"smtp.example.org\"}}}`
	for _, c := range []struct {
		err    error
		fields []string
		words  string
	}{
		{&APIError{Code: 422, Reason: "Invalid", Message: ` "" is invalid: patch: Invalid value: "` + quoted + `": strict decoding error: unknown field "spec.mail"`},
			[]string{"spec.mail"}, `k8s 422 Invalid: strict decoding error: unknown field "spec.mail"`},
		{&APIError{Code: 422, Reason: "Invalid", Message: ` "" is invalid: patch: Invalid value: "` + quoted + `": strict decoding error: unknown field "spec.mail.user", unknown field "spec.mail.x"`},
			[]string{"spec.mail.user", "spec.mail.x"}, `k8s 422 Invalid: strict decoding error: unknown field "spec.mail.user", unknown field "spec.mail.x"`},
		{&APIError{Code: 422, Reason: "Invalid", Message: `Zaentrum.zaentrum.io "zaentrum" is invalid: spec.mail.port: Invalid value: "string": spec.mail.port in body must be of type integer: "string"`}, nil, ""},
		{&APIError{Code: 403, Reason: "Forbidden", Message: `strict decoding error: unknown field "spec.mail"`}, nil, ""},
		{&APIError{Code: 422, Reason: "Invalid", Message: `strict decoding error: duplicate field "host"`}, nil, ""},
		{nil, nil, ""},
	} {
		fields, words := UnknownFields(c.err)
		if !slices.Equal(fields, c.fields) || words != c.words {
			t.Errorf("UnknownFields(%v) = %q, %q; want %q, %q", c.err, fields, words, c.fields, c.words)
		}
	}
}
