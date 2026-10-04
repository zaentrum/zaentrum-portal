package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth/authtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/katalog"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/people"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeSetupStore is the completion record in memory, with the store's rule:
// the first admin to mark setup done is the one it names.
type fakeSetupStore struct {
	mu sync.Mutex
	c  *model.SetupCompletion
}

func (f *fakeSetupStore) SetupCompletion(context.Context) (*model.SetupCompletion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.c == nil {
		return nil, nil
	}
	c := *f.c
	return &c, nil
}

func (f *fakeSetupStore) CompleteSetup(_ context.Context, by string) (model.SetupCompletion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.c == nil {
		f.c = &model.SetupCompletion{At: time.Now().UTC().Truncate(time.Second), By: by}
	}
	return *f.c, nil
}

func (f *fakeSetupStore) ReopenSetup(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.c = nil
	return nil
}

// fakeKatalog is katalog-manager's GraphQL endpoint as the checklist uses it:
// the tmdb.api_key setting, the server's own key, the titles, the scans. It
// records the bearer of every call and refuses one that is not an admin's.
type fakeKatalog struct {
	*httptest.Server
	mu       sync.Mutex
	key      string // tmdb.api_key as set; "" for none
	envKey   bool
	movies   int
	series   int
	scans    []map[string]any
	bearers  []string
	refuse   bool // answer FORBIDDEN, as for a token without the catalog's admin role
	setCalls int
	// blankKey keeps a tmdb.api_key row with no value when none is set, as
	// one cleared by hand leaves it.
	blankKey bool
}

func newFakeKatalog(t *testing.T) *fakeKatalog {
	t.Helper()
	f := &fakeKatalog{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeKatalog) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.bearers = append(f.bearers, r.Header.Get("Authorization"))
	answer := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}
	if f.refuse {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"forbidden: settings requires the catalog-admin role","extensions":{"code":"FORBIDDEN"}}]}`))
		return
	}
	switch {
	case strings.Contains(body.Query, "setSecretSetting"):
		f.setCalls++
		if body.Variables["k"] != katalog.TMDBSetting {
			http.Error(w, "unexpected key", http.StatusBadRequest)
			return
		}
		f.key, _ = body.Variables["v"].(string)
		answer(map[string]any{"setSecretSetting": map[string]any{"key": katalog.TMDBSetting, "isSecret": true, "isSet": true, "updatedAt": "2026-10-04T06:00:00Z"}})
	case strings.Contains(body.Query, "triggerScan"):
		job := map[string]any{"id": fmt.Sprintf("scan-%d", len(f.scans)+1), "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339),
			"finishedAt": nil, "errorMessage": nil, "filesSeen": 0, "itemsInserted": 0, "itemsUpdated": 0}
		f.scans = append([]map[string]any{job}, f.scans...)
		answer(map[string]any{"triggerScan": job})
	default:
		settings := []any{map[string]any{"key": "omdb.api_key", "isSecret": true, "isSet": false, "updatedAt": nil}}
		switch {
		case f.key != "":
			settings = append(settings, map[string]any{"key": katalog.TMDBSetting, "isSecret": true, "isSet": true, "updatedAt": "2026-10-04T06:00:00Z"})
		case f.blankKey:
			settings = append(settings, map[string]any{"key": katalog.TMDBSetting, "isSecret": true, "isSet": false, "updatedAt": "2026-10-01T06:00:00Z"})
		}
		list := func(n int) []any {
			out := make([]any, 0, n)
			for i := 0; i < n && i < katalog.CountLimit; i++ {
				out = append(out, map[string]any{"id": fmt.Sprint(i)})
			}
			return out
		}
		scans := []any{}
		if len(f.scans) > 0 {
			scans = append(scans, f.scans[0])
		}
		answer(map[string]any{
			"settings": settings, "enrichStatus": map[string]any{"tmdbEnabled": f.envKey},
			"movies": list(f.movies), "series": list(f.series), "scanJobs": scans,
		})
	}
}

// finishScan ends the latest scan as katalog-manager's scanner does, with
// what it found.
func (f *fakeKatalog) finishScan(files, inserted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans[0]["status"], f.scans[0]["finishedAt"] = "done", time.Now().UTC().Format(time.RFC3339)
	f.scans[0]["filesSeen"], f.scans[0]["itemsInserted"] = files, inserted
	f.movies += inserted
}

func (f *fakeKatalog) lastBearer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bearers) == 0 {
		return ""
	}
	return f.bearers[len(f.bearers)-1]
}

// setupEnv is portal-api behind the test issuer, with a fake catalog manager,
// the fake apiserver holding the operator's resource, and the completion
// record in memory.
type setupEnv struct {
	*tokenEnv
	kat   *fakeKatalog
	store *fakeSetupStore
}

func newSetupEnv(t *testing.T) *setupEnv {
	t.Helper()
	e := newTokenEnv(t)
	kat := newFakeKatalog(t)
	st := &fakeSetupStore{}
	e.api.setup, e.api.katalog = st, katalog.New(kat.URL)
	e.api.cfg.OIDCIssuer = "http://zaentrum.localhost/auth/realms/zaentrum"
	e.api.cfg.KatalogManagerURL = kat.URL
	// The stock Role: no node reads.
	e.kube.Forbidden["nodes"] = true
	return &setupEnv{tokenEnv: e, kat: kat, store: st}
}

// putPlatform writes the operator's resource as a fresh install has it, with
// the spec fields given on top.
func (e *setupEnv) putPlatform(spec map[string]any, source string) {
	base := map[string]any{
		"version": "latest", "hostname": "zaentrum.localhost",
		"features": map[string]any{"pipeline": false, "gpu": false, "kafka": true},
		"identity": map[string]any{"mode": "bundled", "issuerScheme": "http"},
	}
	for k, v := range spec {
		base[k] = v
	}
	e.kube.Put("zaentrums", map[string]any{"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum"}, "spec": base})
	if source != "" {
		e.kube.SetStatus("zaentrums", "zaentrum", map[string]any{"phase": "Ready",
			"controller": map[string]any{"image": "ghcr.io/zaentrum/operator:v0.4.1", "source": source}})
	}
}

// putWorker seeds one of the pipeline's Deployments, ready or not, and its
// pod — unschedulable when reason says so.
func (e *setupEnv) putWorker(name string, ready bool, reason string) {
	e.kube.Put("deployments", map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "ownerReferences": []any{map[string]any{"kind": "Zaentrum", "name": "zaentrum"}}},
		"spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "ghcr.io/zaentrum/" + name + ":latest"}}}}},
	})
	n := 0
	if ready {
		n = 1
	}
	e.kube.SetStatus("deployments", name, map[string]any{"observedGeneration": 1, "replicas": 1, "readyReplicas": n, "updatedReplicas": 1, "availableReplicas": n})
	status := map[string]any{"phase": "Running"}
	if reason == "Unschedulable" {
		status = map[string]any{"phase": "Pending", "conditions": []any{map[string]any{"type": "PodScheduled", "status": "False",
			"reason": "Unschedulable", "message": "0/1 nodes are available: 1 Insufficient nvidia.com/gpu."}}}
	}
	e.kube.Put("pods", map[string]any{"metadata": map[string]any{"name": name + "-5d9c7b8f6-x2k4p", "labels": map[string]any{"app": name}}, "status": status})
}

// devicesFrom reads the devices step of a request that came in on host over
// scheme, as the ingress in front of portal-api forwards it.
func (e *setupEnv) devicesFrom(scheme, host string) setupDevices {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/setup", nil)
	req.Header.Set("Authorization", "Bearer "+e.iss.Token(e.t, adminPortal))
	req.Header.Set("X-Forwarded-Proto", scheme)
	req.Header.Set("X-Forwarded-Host", host)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /setup from %s://%s = %d %s", scheme, host, rec.Code, rec.Body)
	}
	return decodeSetup(e.t, rec).Devices
}

// setup reads the checklist as an admin of the portal.
func (e *setupEnv) setup() setupDoc {
	e.t.Helper()
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/setup", nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /setup = %d %s", rec.Code, rec.Body)
	}
	return decodeSetup(e.t, rec)
}

func decodeSetup(t *testing.T, rec *httptest.ResponseRecorder) setupDoc {
	t.Helper()
	var doc setupDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("not a setup document: %v %s", err, rec.Body)
	}
	return doc
}

// ─── access ──────────────────────────────────────────────────────────────────

// The checklist and every write of it take the admin role on a token of the
// portal's own clients — the rule of every admin route.
func TestSetupTakesOnlyThePortalsAdmins(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(nil, "")
	for _, rt := range []struct {
		method, path string
		body         any
		ok           int
	}{
		{http.MethodGet, "/api/portal/setup", nil, http.StatusOK},
		{http.MethodGet, "/api/portal/setup/complete", nil, http.StatusOK},
		{http.MethodPost, "/api/portal/setup/complete", nil, http.StatusOK},
		{http.MethodDelete, "/api/portal/setup/complete", nil, http.StatusNoContent},
		{http.MethodPost, "/api/portal/setup/metadata", map[string]any{"tmdbKey": "eyJhbGciOiJIUzI1NiJ9.a-key"}, http.StatusOK},
		{http.MethodPost, "/api/portal/setup/library/scan", nil, http.StatusAccepted},
	} {
		for _, c := range []struct {
			name   string
			claims map[string]any
			want   int
		}{
			{"admin through the portal", adminPortal, rt.ok},
			{"admin through the CLI", adminCLI, rt.ok},
			{"admin through the media app", adminMedia, http.StatusForbidden},
			{"viewer through the portal", viewerPortal, http.StatusForbidden},
			{"an addon's service account", addonSample, http.StatusForbidden},
			{"no bearer", nil, http.StatusUnauthorized},
		} {
			calls := len(e.kat.bearers)
			if rec := e.do(c.claims, rt.method, rt.path, rt.body); rec.Code != c.want {
				t.Errorf("%s %s as %s = %d %s, want %d", rt.method, rt.path, c.name, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
			}
			if c.want >= 400 && len(e.kat.bearers) != calls {
				t.Errorf("%s %s as %s reached the catalog manager", rt.method, rt.path, c.name)
			}
		}
	}
}

// ─── the steps ───────────────────────────────────────────────────────────────

// A fresh appliance: no key, nothing in the library, the pipeline off, plain
// http on a localhost name — every step says what is missing, read from where
// it is configured.
func TestSetupOfAFreshBox(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(nil, "appliance")
	doc := e.setup()

	if doc.Completed != nil {
		t.Errorf("completed = %+v", doc.Completed)
	}
	if m := doc.Metadata; m.State != stepTodo || m.Key != "none" || m.UpdatedAt != nil {
		t.Errorf("metadata = %+v", m)
	}
	l := doc.Library
	if l.State != stepTodo || l.Titles != 0 || l.TitlesMore || l.Scan != nil || !l.Appliance {
		t.Errorf("library = %+v", l)
	}
	// No catalog Deployment to read: the chart's place for the library.
	if l.Path != "/var/lib/katalog/media" || l.Volume != "media" || l.Folder != "media" {
		t.Errorf("library place = %q on %q/%q", l.Path, l.Volume, l.Folder)
	}
	p := doc.Processing
	if p.State != stepOptional || p.Pipeline == nil || *p.Pipeline || !p.Switchable || len(p.Workers) != 0 {
		t.Errorf("processing = %+v", p)
	}
	// Nodes are cluster-scoped and the Role is not: unknown, and why.
	if p.GPUNodes != nil || !strings.Contains(p.GPUNote, "Role") {
		t.Errorf("gpu nodes = %v, note %q", p.GPUNodes, p.GPUNote)
	}
	d := doc.Devices
	if d.State != stepTodo || d.HTTPS || d.IssuerHTTPS || !d.LocalOnly || d.Origin != "http://zaentrum.localhost" || d.Source != "operator" {
		t.Errorf("devices = %+v", d)
	}
	if doc.People.State != stepInfo || doc.People.Mode != peopleExternal {
		t.Errorf("people = %+v", doc.People)
	}
	// The catalog was read with the admin's own bearer, nothing else.
	if b := e.kat.lastBearer(); !strings.HasPrefix(b, "Bearer ") || len(b) < 40 {
		t.Errorf("bearer forwarded = %q", b)
	}
}

// The key goes to the catalog manager's setting with the admin's bearer, and
// nowhere else: not into the answer, not into the log.
func TestSetupSetsTheTMDBKey(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(nil, "")
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	const key = "eyJhbGciOiJIUzI1NiJ9.eyJhdWQiOiJ0bWRiIn0.c2lnbmVkLWJ5LXRtZGI"

	rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/metadata", map[string]any{"tmdbKey": "  " + key + "\n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set key = %d %s", rec.Code, rec.Body)
	}
	if e.kat.key != key {
		t.Errorf("the catalog manager got %q, want the key trimmed", e.kat.key)
	}
	if doc := decodeSetup(t, rec); doc.Metadata.State != stepDone || doc.Metadata.Key != "setting" || doc.Metadata.UpdatedAt == nil {
		t.Errorf("metadata after = %+v", doc.Metadata)
	}
	if strings.Contains(rec.Body.String(), key) || strings.Contains(logged.String(), key) || strings.Contains(logged.String(), "c2lnbmVk") {
		t.Errorf("the key was repeated:\nanswer %s\nlog %s", rec.Body, logged.String())
	}
	if !strings.Contains(logged.String(), "admin set the TMDB key") {
		t.Errorf("the log does not say who set it: %q", logged.String())
	}

	// What no TMDB token is never reaches the catalog manager.
	calls := e.kat.setCalls
	for _, body := range []any{
		map[string]any{"tmdbKey": "   "},
		map[string]any{"tmdbKey": "two words"},
		map[string]any{"tmdbKey": strings.Repeat("x", maxTMDBKey+1)},
		map[string]any{"key": key},
		`{"tmdbKey": 42}`,
	} {
		if rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/metadata", body); rec.Code != http.StatusBadRequest {
			t.Errorf("set %v = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if e.kat.setCalls != calls {
		t.Error("a refused key reached the catalog manager")
	}

	// A key the catalog manager runs with itself counts as one.
	e.kat.key, e.kat.envKey = "", true
	if m := e.setup().Metadata; m.State != stepDone || m.Key != "environment" {
		t.Errorf("metadata with the server's own key = %+v", m)
	}
	// A setting that holds no value is no key.
	e.kat.envKey, e.kat.blankKey = false, true
	if m := e.setup().Metadata; m.State != stepTodo || m.Key != "none" {
		t.Errorf("metadata with a blank setting = %+v", m)
	}
}

// Scan now: the scan starts with the admin's bearer, runs, and the titles it
// found are the library's count.
func TestSetupScansTheLibrary(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(nil, "")
	rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/library/scan", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scan = %d %s", rec.Code, rec.Body)
	}
	l := decodeSetup(t, rec).Library
	if l.State != stepWorking || l.Scan == nil || l.Scan.Status != "running" || l.Scan.ID != "scan-1" {
		t.Errorf("library while scanning = %+v", l)
	}
	e.kat.finishScan(14, 12)
	l = e.setup().Library
	if l.State != stepDone || l.Titles != 12 || l.Scan.Status != "done" || l.Scan.FilesSeen != 14 || l.Scan.ItemsInserted != 12 {
		t.Errorf("library after the scan = %+v %+v", l, l.Scan)
	}
	// A body, when one is sent, must be empty.
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/library/scan", `{"source":"elsewhere"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("scan with a source = %d", rec.Code)
	}
	// A count that reaches the page is a floor.
	e.kat.series = katalog.CountLimit + 5
	if l := e.setup().Library; l.Titles != 12+katalog.CountLimit || !l.TitlesMore {
		t.Errorf("titles = %d, more %v", l.Titles, l.TitlesMore)
	}
}

// Where files go is read from the catalog's own Deployment.
func TestSetupNamesWhereFilesGo(t *testing.T) {
	e := newSetupEnv(t)
	e.api.cfg.KatalogManagerURL = "http://katalog-manager-api"
	e.kube.Put("deployments", map[string]any{
		"metadata": map[string]any{"name": "katalog-manager-api"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "app",
				"env":          []any{map[string]any{"name": "NFS_ROOT", "value": "/var/lib/katalog/library"}},
				"volumeMounts": []any{map[string]any{"name": "data", "mountPath": "/var/lib/katalog"}}}},
			"volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "nas-media"}}},
		}}},
	})
	if l := e.setup().Library; l.Path != "/var/lib/katalog/library" || l.Volume != "nas-media" || l.Folder != "library" {
		t.Errorf("library place = %q on %q/%q", l.Path, l.Volume, l.Folder)
	}
}

// The pipeline: optional while off; on, its workers as they run — a
// transcoder no node can take is the admin's to fix — and the GPU nodes when
// the nodes can be read.
func TestSetupProcessing(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(map[string]any{"features": map[string]any{"pipeline": true, "gpu": true}}, "")

	// Just switched on: the operator has not rolled the workers out yet.
	p := e.setup().Processing
	if p.State != stepWorking || len(p.Workers) != 4 || p.Workers[0].Phase != "absent" || p.Pipeline == nil || !*p.Pipeline || p.GPU == nil || !*p.GPU {
		t.Errorf("processing before the rollout = %+v", p)
	}

	e.putWorker("analyzer", true, "")
	e.putWorker("katalog-ingest", true, "")
	e.putWorker("packager", true, "")
	e.putWorker("transcoder", false, "Unschedulable")
	p = e.setup().Processing
	if p.State != stepTodo {
		t.Errorf("an unschedulable transcoder: state %q, workers %+v", p.State, p.Workers)
	}
	for _, wk := range p.Workers {
		want := ""
		if wk.Name == "transcoder" {
			want = "Unschedulable"
		}
		if wk.Reason != want {
			t.Errorf("%s: reason %q, want %q", wk.Name, wk.Reason, want)
		}
	}

	// Nodes readable after all: one offers a GPU, and the transcoder runs.
	e.kube.Forbidden["nodes"] = false
	e.kube.Put("nodes", map[string]any{"metadata": map[string]any{"name": "gpu-1"}, "status": map[string]any{"capacity": map[string]any{"nvidia.com/gpu": "1"}}})
	e.putWorker("transcoder", true, "")
	p = e.setup().Processing
	if p.State != stepDone || p.GPUNodes == nil || *p.GPUNodes != 1 || p.GPUNote != "" {
		t.Errorf("processing running = %+v (gpu nodes %v)", p, p.GPUNodes)
	}

	// No operator to read: the workers that run say it is on; none, unknown.
	e.kube.Remove("zaentrums", "zaentrum")
	if p = e.setup().Processing; p.State != stepDone || p.Pipeline != nil || p.Switchable {
		t.Errorf("processing without an operator = %+v", p)
	}
	for _, name := range pipelineWorkloads {
		e.kube.Remove("deployments", name)
	}
	if p = e.setup().Processing; p.State != stepUnknown || p.Note == "" {
		t.Errorf("processing with nothing to read = %+v", p)
	}
}

// Phones and TVs sign in over https only: the step is done when the platform
// is reached and signs in over https on a name other devices reach.
func TestSetupDevices(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(map[string]any{"hostname": "media.example.com", "identity": map[string]any{"issuerScheme": "https"}}, "")
	e.api.cfg.OIDCIssuer = "https://media.example.com/auth/realms/zaentrum"
	if d := e.setup().Devices; d.State != stepDone || !d.HTTPS || !d.IssuerHTTPS || d.LocalOnly || d.Origin != "https://media.example.com" {
		t.Errorf("https = %+v", d)
	}
	// Signing in elsewhere, over http: not yet.
	e.api.cfg.OIDCIssuer = "http://sso.example.com/realms/media"
	if d := e.setup().Devices; d.State != stepTodo || !d.HTTPS || d.IssuerHTTPS {
		t.Errorf("an http issuer = %+v", d)
	}
	// Reached through a port-forward, over http: the bundled issuer's scheme
	// is the public host's.
	e.api.cfg.OIDCIssuer = "https://media.example.com/auth/realms/zaentrum"
	if d := e.devicesFrom("http", "localhost:8080"); d.State != stepDone || d.Origin != "https://media.example.com" {
		t.Errorf("through a port-forward = %+v", d)
	}
	// An issuer of its own (a shared realm): the resource's issuerScheme says
	// nothing about the host, and this request's scheme on it does.
	e.putPlatform(map[string]any{"hostname": "media.example.com",
		"identity": map[string]any{"mode": "external", "issuer": "https://sso.example.com/realms/media", "issuerScheme": "http"}}, "")
	e.api.cfg.OIDCIssuer = "https://sso.example.com/realms/media"
	if d := e.devicesFrom("https", "media.example.com"); d.State != stepDone || !d.HTTPS || d.Origin != "https://media.example.com" {
		t.Errorf("external issuer, reached over https = %+v", d)
	}
	if d := e.devicesFrom("http", "media.example.com"); d.State != stepTodo || d.HTTPS {
		t.Errorf("external issuer, reached over http = %+v", d)
	}
	// Without an operator, the address this request came in on.
	e.kube.Remove("zaentrums", "zaentrum")
	e.api.cfg.OIDCIssuer = "https://media.example.com/auth/realms/zaentrum"
	if d := e.devicesFrom("https", "media.example.com"); d.State != stepDone || d.Source != "request" || d.Origin != "https://media.example.com" {
		t.Errorf("from the request = %+v", d)
	}
	for host, local := range map[string]bool{"localhost": true, "zaentrum.localhost": true, "127.0.0.1": true, "[::1]": true, "media.example.com": false, "192.168.1.20": false} {
		if got := devicesStep(nil, "https://media.example.com/auth/realms/zaentrum", "https://"+host).LocalOnly; got != local {
			t.Errorf("%s: localOnly = %v, want %v", host, got, local)
		}
	}
}

// Done is a record: who marked it, when, kept as the first one marked it —
// and reopened, it is gone and the checklist shows again.
func TestSetupCompleteAndReopen(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(nil, "")
	completion := func() *model.SetupCompletion {
		t.Helper()
		rec := e.do(adminPortal, http.MethodGet, "/api/portal/setup/complete", nil)
		var out struct {
			Completed *model.SetupCompletion `json:"completed"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("GET /setup/complete = %d %s", rec.Code, rec.Body)
		}
		return out.Completed
	}
	if c := completion(); c != nil {
		t.Fatalf("fresh: %+v", c)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/complete", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"by":"admin"`) {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body)
	}
	first := completion()
	if first == nil || first.By != "admin" {
		t.Fatalf("completed = %+v", first)
	}
	if doc := e.setup(); doc.Completed == nil || doc.Completed.By != "admin" {
		t.Errorf("the checklist says completed = %+v", doc.Completed)
	}
	// Marked again, by another admin: the record stays the first.
	other := authtest.Person("zae", "other", "zaentrum-admin")
	if rec := e.do(other, http.MethodPost, "/api/portal/setup/complete", nil); rec.Code != http.StatusOK {
		t.Fatalf("complete again = %d", rec.Code)
	}
	if c := completion(); c == nil || *c != *first {
		t.Errorf("completed again = %+v, want %+v", c, first)
	}
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/setup/complete", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("reopen = %d", rec.Code)
	}
	if c := completion(); c != nil {
		t.Errorf("reopened: %+v", c)
	}
}

// A catalog manager that does not answer, refuses this admin, or is none at
// all costs the checklist its two catalog steps — unknown, saying why — and
// never the rest; a write to it says the same, 502 or 503.
func TestSetupWithoutACatalog(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(nil, "")

	e.kat.refuse = true
	doc := e.setup()
	if doc.Metadata.State != stepUnknown || !strings.Contains(doc.Metadata.Note, "refused this admin") || doc.Library.State != stepUnknown {
		t.Errorf("refused: metadata %+v, library %+v", doc.Metadata, doc.Library)
	}
	if doc.Devices.State != stepTodo || doc.Processing.State != stepOptional {
		t.Errorf("the other steps went with it: %+v %+v", doc.Devices, doc.Processing)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/library/scan", nil); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "refused") {
		t.Errorf("scan refused = %d %s", rec.Code, rec.Body)
	}

	e.kat.Close()
	doc = e.setup()
	if !strings.Contains(doc.Metadata.Note, "did not answer") || !strings.Contains(doc.Library.Note, "did not answer") {
		t.Errorf("unreachable: %+v %+v", doc.Metadata, doc.Library)
	}
	// Where files go is still said.
	if doc.Library.Path == "" {
		t.Errorf("library = %+v", doc.Library)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/metadata", map[string]any{"tmdbKey": "eyJ.a.b"}); rec.Code != http.StatusBadGateway {
		t.Errorf("set key unreachable = %d", rec.Code)
	}

	e.api.katalog = nil
	doc = e.setup()
	if doc.Metadata.State != stepUnknown || !strings.Contains(doc.Metadata.Note, "PORTAL_KATALOG_MANAGER_URL") {
		t.Errorf("no catalog manager: %+v", doc.Metadata)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/setup/library/scan", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("scan without a catalog manager = %d", rec.Code)
	}
}

// The people step says where people get their accounts: the People page with
// the platform's own realm (the step links there), the identity provider's
// console with an external one, and what the page needs when its client has
// no secret. It is never a step to tick off.
func TestSetupSaysWherePeopleAreManaged(t *testing.T) {
	e := newSetupEnv(t)
	e.putPlatform(nil, "appliance")
	e.api.cfg.OIDCIssuer = "https://sso.example.org/realms/household"
	if p := e.setup().People; p.State != stepInfo || p.Mode != peopleExternal || p.ManageURL != "https://sso.example.org/admin/household/console/" {
		t.Errorf("external: %+v", p)
	}
	e.api.people = people.New(keycloak.New(keycloak.Config{URL: "http://keycloak.invalid/auth", Realm: "zaentrum", ClientID: "zaentrum-people"}),
		"zaentrum-admin", "zaentrum-user", nil)
	if p := e.setup().People; p.State != stepInfo || p.Mode != peopleUnavailable || !strings.Contains(p.Note, "zaentrum-people") {
		t.Errorf("no secret: %+v", p)
	}
	e.api.people = people.New(keycloak.New(keycloak.Config{URL: "http://keycloak.invalid/auth", Realm: "zaentrum", ClientID: "zaentrum-people", ClientSecret: "s"}),
		"zaentrum-admin", "zaentrum-user", nil)
	if p := e.setup().People; p.State != stepInfo || p.Mode != peopleBundled || p.ManageURL != "" || p.Note != "" {
		t.Errorf("bundled: %+v", p)
	}
}
