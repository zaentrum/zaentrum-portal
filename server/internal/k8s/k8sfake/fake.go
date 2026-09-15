// Package k8sfake is an in-memory apiserver for tests. It serves namespaced
// custom resources and Secrets with the semantics portal-api relies on:
// resourceVersion conflicts on update, generation bumps on spec changes, JSON
// merge patch, and owner-reference garbage collection on delete.
//
// Reading a Secret — get, list or watch — fails the test. portal-api writes
// secret inputs and never reads them back; this is where that is enforced.
package k8sfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// Call is one request the fake served.
type Call struct {
	Method, Path, ContentType, Body string
}

// Server is the fake apiserver.
type Server struct {
	*httptest.Server
	t testing.TB

	mu      sync.Mutex
	rv      int
	objects map[string]map[string]any // "<plural>/<name>" → object
	secrets map[string]map[string]any // name → Secret
	calls   []Call

	// Unserved plurals answer like a cluster without that CRD: a plain 404.
	Unserved map[string]bool
	// Forbidden plurals (or "secrets") answer 403, like a Role without them.
	Forbidden map[string]bool
}

// New starts a fake apiserver that stops with the test.
func New(t testing.TB) *Server {
	s := &Server{
		t:         t,
		objects:   map[string]map[string]any{},
		secrets:   map[string]map[string]any{},
		Unserved:  map[string]bool{},
		Forbidden: map[string]bool{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Client is a k8s client for this server, acting in namespace.
func (s *Server) Client(namespace string) *k8s.Client {
	return k8s.NewAt(s.URL, namespace, "test-token", s.Server.Client())
}

// Put stores a custom resource as-is (a test's seed), filling generation and
// resourceVersion when absent.
func (s *Server) Put(plural string, obj map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj = clone(obj)
	md := meta(obj)
	if _, ok := md["generation"]; !ok {
		md["generation"] = json.Number("1")
	}
	s.rv++
	md["resourceVersion"] = strconv.Itoa(s.rv)
	s.objects[plural+"/"+str(md["name"])] = obj
}

// Object is a copy of a stored custom resource; nil when absent.
func (s *Server) Object(plural, name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objects[plural+"/"+name]; ok {
		return clone(o)
	}
	return nil
}

// Remove deletes a custom resource the way someone else would — kubectl, a
// deploy repository — including garbage collection of what it owns.
func (s *Server) Remove(plural, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objects[plural+"/"+name]; ok {
		delete(s.objects, plural+"/"+name)
		s.collect(str(o["kind"]), name)
	}
}

// SetStatus replaces a custom resource's status, as its controller would:
// resourceVersion moves, generation does not.
func (s *Server) SetStatus(plural, name string, status map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[plural+"/"+name]
	if !ok {
		s.t.Fatalf("k8sfake: SetStatus on missing %s/%s", plural, name)
	}
	o["status"] = clone(status)
	s.rv++
	meta(o)["resourceVersion"] = strconv.Itoa(s.rv)
}

// Secret is a copy of a stored Secret for a test to inspect; nil when absent.
func (s *Server) Secret(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.secrets[name]; ok {
		return clone(o)
	}
	return nil
}

// PutSecret seeds a Secret.
func (s *Server) PutSecret(obj map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj = clone(obj)
	s.secrets[str(meta(obj)["name"])] = obj
}

// Calls is every request served so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Body: string(body)})

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	// /api/v1/namespaces/<ns>/secrets[/<name>]
	case len(parts) >= 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" && parts[4] == "secrets":
		name := ""
		if len(parts) == 6 {
			name = parts[5]
		}
		s.serveSecret(w, r, name, body)
	// /apis/<group>/<version>/namespaces/<ns>/<plural>[/<name>]
	case len(parts) >= 6 && parts[0] == "apis" && parts[3] == "namespaces":
		name := ""
		if len(parts) == 7 {
			name = parts[6]
		}
		s.serveResource(w, r, parts[5], name, body)
	default:
		http.Error(w, "404 page not found", http.StatusNotFound)
	}
}

func (s *Server) serveSecret(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	if r.Method == http.MethodGet || r.URL.Query().Has("watch") {
		s.t.Errorf("k8sfake: portal-api must never read a Secret: %s %s", r.Method, r.URL)
		status(w, http.StatusForbidden, "Forbidden", "secrets are write-only for portal-api")
		return
	}
	if s.Forbidden["secrets"] {
		status(w, http.StatusForbidden, "Forbidden", "secrets is forbidden")
		return
	}
	switch {
	case r.Method == http.MethodPost && name == "":
		var obj map[string]any
		if err := decode(body, &obj); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		n := str(meta(obj)["name"])
		if _, ok := s.secrets[n]; ok {
			status(w, http.StatusConflict, "AlreadyExists", fmt.Sprintf("secrets %q already exists", n))
			return
		}
		s.secrets[n] = obj
		writeJSON(w, http.StatusCreated, obj)
	case r.Method == http.MethodPatch && name != "":
		cur, ok := s.secrets[name]
		if !ok {
			status(w, http.StatusNotFound, "NotFound", fmt.Sprintf("secrets %q not found", name))
			return
		}
		if r.Header.Get("Content-Type") != "application/merge-patch+json" {
			status(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "k8sfake speaks merge patch only")
			return
		}
		var patch any
		if err := decode(body, &patch); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		next, _ := MergePatch(cur, patch).(map[string]any)
		s.secrets[name] = next
		writeJSON(w, http.StatusOK, next)
	case r.Method == http.MethodDelete && name != "":
		if _, ok := s.secrets[name]; !ok {
			status(w, http.StatusNotFound, "NotFound", fmt.Sprintf("secrets %q not found", name))
			return
		}
		delete(s.secrets, name)
		writeJSON(w, http.StatusOK, map[string]any{"kind": "Status", "status": "Success"})
	default:
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) serveResource(w http.ResponseWriter, r *http.Request, plural, name string, body []byte) {
	if s.Unserved[plural] {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	if s.Forbidden[plural] {
		status(w, http.StatusForbidden, "Forbidden", plural+" is forbidden")
		return
	}
	key := plural + "/" + name
	switch {
	case r.Method == http.MethodGet && name == "":
		keys := make([]string, 0, len(s.objects))
		for k := range s.objects {
			if strings.HasPrefix(k, plural+"/") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		items := make([]any, 0, len(keys))
		for _, k := range keys {
			items = append(items, s.objects[k])
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case r.Method == http.MethodGet:
		o, ok := s.objects[key]
		if !ok {
			status(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", plural, name))
			return
		}
		writeJSON(w, http.StatusOK, o)
	case r.Method == http.MethodPost && name == "":
		var obj map[string]any
		if err := decode(body, &obj); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		md := meta(obj)
		n := str(md["name"])
		if _, ok := s.objects[plural+"/"+n]; ok {
			status(w, http.StatusConflict, "AlreadyExists", fmt.Sprintf("%s %q already exists", plural, n))
			return
		}
		delete(obj, "status")
		md["generation"] = json.Number("1")
		s.rv++
		md["resourceVersion"] = strconv.Itoa(s.rv)
		s.objects[plural+"/"+n] = obj
		writeJSON(w, http.StatusCreated, obj)
	case r.Method == http.MethodPut && name != "":
		cur, ok := s.objects[key]
		if !ok {
			status(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", plural, name))
			return
		}
		var obj map[string]any
		if err := decode(body, &obj); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		md := meta(obj)
		switch rv := str(md["resourceVersion"]); {
		case rv == "":
			status(w, http.StatusUnprocessableEntity, "Invalid", "metadata.resourceVersion: must be specified for an update")
			return
		case rv != str(meta(cur)["resourceVersion"]):
			status(w, http.StatusConflict, "Conflict", "the object has been modified; please apply your changes to the latest version and try again")
			return
		}
		s.store(key, cur, obj)
		writeJSON(w, http.StatusOK, s.objects[key])
	case r.Method == http.MethodPatch && name != "":
		cur, ok := s.objects[key]
		if !ok {
			status(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", plural, name))
			return
		}
		var patch any
		if err := decode(body, &patch); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		next, _ := MergePatch(cur, patch).(map[string]any)
		s.store(key, cur, next)
		writeJSON(w, http.StatusOK, s.objects[key])
	case r.Method == http.MethodDelete && name != "":
		cur, ok := s.objects[key]
		if !ok {
			status(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", plural, name))
			return
		}
		delete(s.objects, key)
		s.collect(str(cur["kind"]), name)
		writeJSON(w, http.StatusOK, map[string]any{"kind": "Status", "status": "Success"})
	default:
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

// store writes next over cur as an update does: status stays the
// controller's, generation moves only when the spec changed.
func (s *Server) store(key string, cur, next map[string]any) {
	next = clone(next)
	next["status"] = cur["status"]
	if next["status"] == nil {
		delete(next, "status")
	}
	md := meta(next)
	gen, _ := strconv.ParseInt(fmt.Sprint(meta(cur)["generation"]), 10, 64)
	if !reflect.DeepEqual(normal(cur["spec"]), normal(next["spec"])) {
		gen++
	}
	md["generation"] = json.Number(strconv.FormatInt(gen, 10))
	s.rv++
	md["resourceVersion"] = strconv.Itoa(s.rv)
	s.objects[key] = next
}

// collect is garbage collection: Secrets owned by the deleted object go.
func (s *Server) collect(kind, name string) {
	for n, sec := range s.secrets {
		refs, _ := meta(sec)["ownerReferences"].([]any)
		for _, ref := range refs {
			m, _ := ref.(map[string]any)
			if str(m["kind"]) == kind && str(m["name"]) == name {
				delete(s.secrets, n)
				break
			}
		}
	}
}

// MergePatch applies an RFC 7386 JSON merge patch.
func MergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	out := map[string]any{}
	if t, ok := target.(map[string]any); ok {
		for k, v := range t {
			out[k] = v
		}
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = MergePatch(out[k], v)
	}
	return out
}

func meta(obj map[string]any) map[string]any {
	md, ok := obj["metadata"].(map[string]any)
	if !ok {
		md = map[string]any{}
		obj["metadata"] = md
	}
	return md
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// Generation is a stored custom resource's metadata.generation; 0 when absent.
func (s *Server) Generation(plural, name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[plural+"/"+name]
	if !ok {
		return 0
	}
	gen, _ := strconv.ParseInt(fmt.Sprint(meta(o)["generation"]), 10, 64)
	return gen
}

// decode keeps numbers exact (json.Number), like the apiserver does.
func decode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

// clone deep-copies a JSON object through encoding.
func clone(obj map[string]any) map[string]any {
	b, _ := json.Marshal(obj)
	var out map[string]any
	_ = decode(b, &out)
	return out
}

// normal re-decodes a JSON value so that equal documents compare equal.
func normal(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = decode(b, &out)
	return out
}

func status(w http.ResponseWriter, code int, reason, message string) {
	writeJSON(w, code, map[string]any{"kind": "Status", "status": "Failure", "reason": reason, "message": message, "code": code})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
