package api

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/katalog"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// ─── first-run setup ─────────────────────────────────────────────────────────
//
// A fresh install had no first run of its own. An admin signed in with a
// one-time password, had to find Catalog Management's settings to add a TMDB
// key, copy files and press scan — and nothing said what else was missing.
//
// The setup checklist is what the launchpad shows an admin until one marks it
// done. Each step is read live from where it is configured — the catalog
// (with the admin's own bearer), the operator's resource, the cluster,
// portal-api's own configuration — so it cannot drift from the platform, and
// none of it is stored here: only the record that setup was marked done.
//
//	metadata    a TMDB key in effect
//	library     titles in the catalog, the latest scan, where files go
//	processing  the media pipeline, its workers, whether a node offers a GPU
//	devices     https, which phones and TVs sign in over only
//	people      accounts — nothing to check; where they are managed: the
//	            People page with bundled identity, else the identity provider

// Step states.
const (
	stepDone     = "done"     // nothing left to do
	stepTodo     = "todo"     // an admin has something to do
	stepWorking  = "working"  // under way: a scan running, workers starting
	stepOptional = "optional" // off, and fine off
	stepUnknown  = "unknown"  // portal-api cannot tell; note says why
	stepInfo     = "info"     // nothing to check from here
)

// pipelineWorkloads are the media pipeline's Deployments, as the platform
// chart names them.
var pipelineWorkloads = []string{"analyzer", "katalog-ingest", "packager", "transcoder"}

// maxTMDBKey bounds a key: a TMDB read access token is a few hundred bytes.
const maxTMDBKey = 4096

// setupStore is the completion record as the setup endpoints use it.
// *store.Store implements it; tests substitute an in-memory one.
type setupStore interface {
	SetupCompletion(ctx context.Context) (*model.SetupCompletion, error)
	CompleteSetup(ctx context.Context, by string) (model.SetupCompletion, error)
	ReopenSetup(ctx context.Context) error
}

// katalogAPI is katalog-manager as the setup endpoints call it.
// *katalog.Client implements it; nil when none is configured.
type katalogAPI interface {
	Overview(ctx context.Context, bearer string) (katalog.Overview, error)
	SetSecretSetting(ctx context.Context, bearer, key, value string) (katalog.Setting, error)
	TriggerScan(ctx context.Context, bearer string) (katalog.ScanJob, error)
}

// setupDoc is GET /api/portal/setup.
type setupDoc struct {
	// Completed is the record that setup was marked done; null while it is
	// not, and the launchpad shows the checklist to admins until then.
	Completed  *model.SetupCompletion `json:"completed"`
	Metadata   setupMetadata          `json:"metadata"`
	Library    setupLibrary           `json:"library"`
	Processing setupProcessing        `json:"processing"`
	Devices    setupDevices           `json:"devices"`
	People     setupPeople            `json:"people"`
}

// setupStep is what every step says: its state, and why when it is unknown.
type setupStep struct {
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

type setupMetadata struct {
	setupStep
	// Key is where the TMDB key in effect comes from: setting (tmdb.api_key,
	// set here or in Catalog Management), environment (the catalog manager's
	// own TMDB_API_KEY), or none. Never the key.
	Key string `json:"key"`
	// UpdatedAt is when the setting was last written.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

type setupLibrary struct {
	setupStep
	// Titles is how many titles the catalog holds, movies and series; at
	// least that many when TitlesMore is set (each kind is counted to 200).
	Titles     int  `json:"titles"`
	TitlesMore bool `json:"titlesMore"`
	// Path is where the catalog reads the library, as it sees it; Volume is
	// the claim holding it and Folder the folder inside that claim — files
	// copied there are what a scan finds. Volume is "" when it cannot be told.
	Path   string `json:"path"`
	Volume string `json:"volume"`
	Folder string `json:"folder"`
	// Appliance: the platform runs in the one-container appliance, where the
	// volume is a directory inside the container.
	Appliance bool `json:"appliance"`
	// Scan is the latest scan; null when none ran.
	Scan *katalog.ScanJob `json:"scan"`
}

type setupProcessing struct {
	setupStep
	// Pipeline and GPU are what the operator's resource asks for
	// (spec.features); null without one to read.
	Pipeline *bool `json:"pipeline"`
	GPU      *bool `json:"gpu"`
	// GPUNodes is how many nodes offer an NVIDIA GPU, which the transcoder
	// needs; null when the nodes cannot be read, and GPUNote says why.
	GPUNodes *int   `json:"gpuNodes"`
	GPUNote  string `json:"gpuNote,omitempty"`
	// Workers are the pipeline's workloads as they run.
	Workers []setupWorker `json:"workers"`
	// Switchable: there is an operator's resource to switch the pipeline on
	// — PATCH /api/portal/operator {"pipeline": true|false}.
	Switchable bool `json:"switchable"`
}

// setupWorker is one of the pipeline's workloads.
type setupWorker struct {
	Name string `json:"name"`
	// Phase is the operator console's (ready, progressing, degraded,
	// stopped), or absent when the platform runs no such workload.
	Phase   string `json:"phase"`
	Reason  string `json:"reason"`
	Ready   int    `json:"ready"`
	Desired int    `json:"desired"`
}

// setupPeople is where the people who use the server get their accounts. Its
// state is info: nothing to check, one account per person.
type setupPeople struct {
	setupStep
	// Mode is GET /people's: bundled (the People page manages them),
	// external (they live in the identity provider at ManageURL), or
	// unavailable (the People page is not set up; Note says why).
	Mode      string `json:"mode"`
	ManageURL string `json:"manageUrl,omitempty"`
}

type setupDevices struct {
	setupStep
	// Origin is where the platform is reached, and HTTPS whether over https;
	// Issuer is where it signs in, and IssuerHTTPS whether over https. Phones
	// and TVs need both.
	Origin      string `json:"origin"`
	HTTPS       bool   `json:"https"`
	Issuer      string `json:"issuer"`
	IssuerHTTPS bool   `json:"issuerHttps"`
	// LocalOnly: a localhost name, which only this machine reaches.
	LocalOnly bool `json:"localOnly"`
	// Source says where Origin was read: operator (its resource's hostname)
	// or request (the address this request came in on).
	Source string `json:"source"`
}

// ─── handlers ────────────────────────────────────────────────────────────────

// getSetup handles GET /api/portal/setup: every step, read now.
func (a *API) getSetup(w http.ResponseWriter, r *http.Request) {
	doc, err := a.setupDocument(r)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// setupCompletion handles GET /api/portal/setup/complete: only the record,
// which is what the launchpad asks before it reads the steps.
func (a *API) setupCompletion(w http.ResponseWriter, r *http.Request) {
	c, err := a.setup.SetupCompletion(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"completed": c})
}

// completeSetup handles POST /api/portal/setup/complete: setup is marked
// done, by the caller. Marked already, the record stays as it was.
func (a *API) completeSetup(w http.ResponseWriter, r *http.Request) {
	c, err := a.setup.CompleteSetup(r.Context(), callerName(r))
	if err != nil {
		serverError(w, err)
		return
	}
	log.Printf("setup: %s marked setup done", requester(r))
	writeJSON(w, http.StatusOK, map[string]any{"completed": c})
}

// reopenSetup handles DELETE /api/portal/setup/complete: the record goes, and
// the launchpad shows the checklist again.
func (a *API) reopenSetup(w http.ResponseWriter, r *http.Request) {
	if err := a.setup.ReopenSetup(r.Context()); err != nil {
		serverError(w, err)
		return
	}
	log.Printf("setup: %s reopened setup", requester(r))
	w.WriteHeader(http.StatusNoContent)
}

// setTMDBKey handles POST /api/portal/setup/metadata {"tmdbKey": "…"}: the
// key goes to katalog-manager's tmdb.api_key setting, with the admin's
// bearer, and is never stored, logged or answered here. The answer is the
// checklist, read again.
func (a *API) setTMDBKey(w http.ResponseWriter, r *http.Request) {
	if !a.katalogReady(w) {
		return
	}
	var body struct {
		TMDBKey string `json:"tmdbKey"`
	}
	if !decode(w, r, &body) {
		return
	}
	key := strings.TrimSpace(body.TMDBKey)
	switch {
	case key == "":
		badRequest(w, "tmdbKey is empty — paste the API read access token from your TMDB account's API settings")
		return
	case len(key) > maxTMDBKey:
		badRequest(w, "tmdbKey is longer than any TMDB token")
		return
	case strings.ContainsAny(key, " \t\r\n"):
		badRequest(w, "tmdbKey holds spaces or line breaks, which no TMDB token does — paste it again")
		return
	}
	if _, err := a.katalog.SetSecretSetting(r.Context(), r.Header.Get("Authorization"), katalog.TMDBSetting, key); err != nil {
		katalogFailure(w, err)
		return
	}
	log.Printf("setup: %s set the TMDB key", requester(r))
	a.answerSetup(w, r, http.StatusOK)
}

// startScan handles POST /api/portal/setup/library/scan: katalog-manager
// starts a scan, with the admin's bearer. The answer is the checklist, read
// again, the scan running in it.
func (a *API) startScan(w http.ResponseWriter, r *http.Request) {
	if !a.katalogReady(w) {
		return
	}
	var body struct{}
	if !decodeOptional(w, r, &body) {
		return
	}
	job, err := a.katalog.TriggerScan(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		katalogFailure(w, err)
		return
	}
	log.Printf("setup: %s started scan %s", requester(r), job.ID)
	a.answerSetup(w, r, http.StatusAccepted)
}

// answerSetup answers a write with the checklist as it reads now.
func (a *API) answerSetup(w http.ResponseWriter, r *http.Request, status int) {
	doc, err := a.setupDocument(r)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, status, doc)
}

// katalogReady refuses a write to the catalog when there is no catalog
// manager to write to.
func (a *API) katalogReady(w http.ResponseWriter) bool {
	if a.katalog == nil {
		http.Error(w, noKatalog, http.StatusServiceUnavailable)
		return false
	}
	return true
}

// katalogFailure answers a write katalog-manager did not take: 502, in its
// words — portal-api took the request; the catalog manager did not.
func katalogFailure(w http.ResponseWriter, err error) {
	http.Error(w, katalogNote(err), http.StatusBadGateway)
}

// katalogNote says what became of a call to katalog-manager.
func katalogNote(err error) string {
	var refused *katalog.Refused
	var failed *katalog.Failed
	switch {
	case errors.Is(err, katalog.ErrUnreachable):
		return "the catalog manager did not answer: " + strings.TrimPrefix(err.Error(), katalog.ErrUnreachable.Error()+": ")
	case errors.As(err, &refused):
		return "the catalog manager refused this admin: " + refused.Message
	case errors.As(err, &failed):
		return "the catalog manager answered: " + failed.Message
	}
	return err.Error()
}

// callerName is who the completion record names: the username, else the
// subject.
func callerName(r *http.Request) string {
	if p, _ := auth.PrincipalFrom(r.Context()); p != nil {
		if p.Username != "" {
			return p.Username
		}
		return p.Subject
	}
	return ""
}

// ─── the document ────────────────────────────────────────────────────────────

// setupDocument reads every step. The reads run side by side: each has its
// own timeout, and one that does not answer costs the checklist that step.
func (a *API) setupDocument(r *http.Request) (setupDoc, error) {
	ctx := r.Context()
	completed, err := a.setup.SetupCompletion(ctx)
	if err != nil {
		return setupDoc{}, err
	}

	var (
		wg           sync.WaitGroup
		overview     katalog.Overview
		overviewErr  error
		platform     *operator.Platform
		platformNote = "instance management is unavailable (not running in a cluster)"
		gpus         gpuNodes
		media        = operator.MediaVolume{Path: operator.DefaultMediaPath, Claim: "media", Folder: "media"}
		instances    []operator.Instance
		instancesErr = errors.New("not running in a cluster")
	)
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}
	if a.katalog != nil {
		run(func() { overview, overviewErr = a.katalog.Overview(ctx, r.Header.Get("Authorization")) })
	}
	if a.op != nil && a.op.Available() {
		run(func() { platform, platformNote = a.op.Platform(ctx) })
		run(func() { gpus.n, gpus.known, gpus.note = a.op.GPUNodes(ctx) })
		run(func() { media = a.op.MediaVolume(ctx, installHost(a.cfg.KatalogManagerURL)) })
		run(func() { instances, instancesErr = a.op.Instances(ctx) })
	}
	wg.Wait()

	return setupDoc{
		Completed:  completed,
		Metadata:   metadataStep(a.katalog != nil, overview, overviewErr),
		Library:    libraryStep(a.katalog != nil, overview, overviewErr, media, platform),
		Processing: processingStep(platform, platformNote, gpus, instances, instancesErr),
		Devices:    devicesStep(platform, a.cfg.OIDCIssuer, requestOrigin(r)),
		People:     a.peopleStep(),
	}, nil
}

// peopleStep says where people get their accounts.
func (a *API) peopleStep() setupPeople {
	out := setupPeople{setupStep: setupStep{State: stepInfo}, Mode: a.peopleMode()}
	switch out.Mode {
	case peopleExternal:
		out.ManageURL = manageURL(a.cfg.PeopleManageURL, a.cfg.OIDCIssuer)
	case peopleUnavailable:
		out.Note = notSetUp
	}
	return out
}

// noKatalog is the note of a step read from a catalog manager there is none of.
const noKatalog = "portal-api is pointed at no catalog manager (PORTAL_KATALOG_MANAGER_URL)"

func metadataStep(configured bool, o katalog.Overview, err error) setupMetadata {
	switch {
	case !configured:
		return setupMetadata{setupStep: setupStep{State: stepUnknown, Note: noKatalog}, Key: "none"}
	case err != nil:
		return setupMetadata{setupStep: setupStep{State: stepUnknown, Note: katalogNote(err)}, Key: "none"}
	}
	switch {
	case o.TMDBKey != nil && o.TMDBKey.IsSet:
		return setupMetadata{setupStep: setupStep{State: stepDone}, Key: "setting", UpdatedAt: o.TMDBKey.UpdatedAt}
	case o.EnvironmentKey:
		return setupMetadata{setupStep: setupStep{State: stepDone}, Key: "environment"}
	}
	return setupMetadata{setupStep: setupStep{State: stepTodo}, Key: "none"}
}

func libraryStep(configured bool, o katalog.Overview, err error, media operator.MediaVolume, p *operator.Platform) setupLibrary {
	out := setupLibrary{
		Path: media.Path, Volume: media.Claim, Folder: media.Folder,
		Appliance: p != nil && p.ControllerSource == operator.SourceAppliance,
	}
	switch {
	case !configured:
		out.setupStep = setupStep{State: stepUnknown, Note: noKatalog}
		return out
	case err != nil:
		out.setupStep = setupStep{State: stepUnknown, Note: katalogNote(err)}
		return out
	}
	out.Titles, out.TitlesMore, out.Scan = o.Titles(), o.More, o.LastScan
	switch {
	case o.LastScan != nil && o.LastScan.Status == "running":
		out.State = stepWorking
	case o.Titles() > 0:
		out.State = stepDone
	default:
		out.State = stepTodo
	}
	return out
}

// gpuNodes is what GPUNodes answered.
type gpuNodes struct {
	n     int
	known bool
	note  string
}

func processingStep(p *operator.Platform, platformNote string, gpus gpuNodes, instances []operator.Instance, instancesErr error) setupProcessing {
	out := setupProcessing{Workers: []setupWorker{}, Switchable: p != nil}
	if gpus.known {
		out.GPUNodes = ptr(gpus.n)
	} else {
		out.GPUNote = gpus.note
	}
	if p != nil {
		out.Pipeline, out.GPU = ptr(p.Pipeline), ptr(p.GPU)
	}
	live := map[string]operator.Instance{}
	for _, in := range instances {
		live[in.Name] = in
	}
	running := false
	for _, name := range pipelineWorkloads {
		in, ok := live[name]
		switch {
		case ok:
			running = true
			out.Workers = append(out.Workers, setupWorker{
				Name: name, Phase: in.Phase, Reason: in.Reason, Ready: in.ReadyReplicas, Desired: in.DesiredReplicas,
			})
		case p != nil && p.Pipeline && instancesErr == nil:
			// Asked for and not running yet: the operator rolls it out.
			out.Workers = append(out.Workers, setupWorker{Name: name, Phase: "absent"})
		}
	}
	// On: the operator's resource says so — or, with none to read, the
	// pipeline's workloads run.
	on := running
	switch {
	case p != nil:
		on = p.Pipeline
	case !running:
		out.setupStep = setupStep{State: stepUnknown, Note: platformNote}
		return out
	}
	if !on {
		out.State = stepOptional
		return out
	}
	if instancesErr != nil {
		out.setupStep = setupStep{State: stepUnknown, Note: "the platform's workloads could not be listed: " + instancesErr.Error()}
		return out
	}
	out.State = stepDone
	for _, wk := range out.Workers {
		switch {
		case wk.Reason != "":
			// A fault the cluster names — Unschedulable, ImagePullBackOff —
			// is the admin's to fix.
			out.State = stepTodo
		case wk.Phase != "ready" && out.State == stepDone:
			out.State = stepWorking
		}
	}
	return out
}

// devicesStep: phones and TVs need the platform reached over https, on a name
// they reach, and sign-in over https. Where it is reached is the operator's
// hostname, else the host this request came in on. Whether over https is
// this request's own scheme when it came in on that host — the ingress or the
// proxy in front of it says so — and otherwise, with the bundled identity,
// the resource's issuerScheme, which the operator derives the bundled
// Keycloak's address on that same host from. With an issuer of its own, the
// resource's issuerScheme says nothing about the platform's host.
func devicesStep(p *operator.Platform, issuer, requested string) setupDevices {
	out := setupDevices{Issuer: issuer, Source: "request", Origin: requested}
	if p != nil && p.Hostname != "" {
		scheme := "http"
		if r, err := url.Parse(requested); err == nil && r.Host != "" {
			scheme = r.Scheme
			if !strings.EqualFold(r.Hostname(), hostnameOf(p.Hostname)) && p.Issuer == "" && p.IdentityMode != "external" {
				scheme = p.Scheme()
			}
		} else if p.Issuer == "" && p.IdentityMode != "external" {
			scheme = p.Scheme()
		}
		out.Source, out.Origin = "operator", scheme+"://"+p.Hostname
		if out.Issuer == "" {
			out.Issuer = p.DerivedIssuer()
		}
	}
	u, err := url.Parse(out.Origin)
	if out.Origin == "" || err != nil || u.Host == "" {
		out.setupStep = setupStep{State: stepUnknown, Note: "the address the platform is reached at cannot be told"}
		return out
	}
	out.HTTPS = u.Scheme == "https"
	out.IssuerHTTPS = strings.HasPrefix(strings.ToLower(out.Issuer), "https://")
	out.LocalOnly = localHost(u.Hostname())
	out.State = stepTodo
	if out.HTTPS && out.IssuerHTTPS && !out.LocalOnly {
		out.State = stepDone
	}
	return out
}

// hostnameOf is a host without its port.
func hostnameOf(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// localHost reports whether a host is this machine's alone: a localhost name
// or a loopback address, which no phone or TV reaches.
func localHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
