package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// Addons as Helm charts.
//
// An admin adds an addon by its chart — a reference, optionally a version or a
// digest, non-secret values and secret inputs. portal-api writes a
// ZaentrumAddon with suspend: true, so the operator fetches and renders the
// chart and reports a plan without applying anything. Installing turns suspend
// off, and only once the plan for the current generation is clean. When the
// operator reports the addon ready, the registration loop registers it from its
// primary Service exactly like an addon added by address
// (addonregistration.go). Removing deletes the resource — the operator and
// garbage collection take what it owns — and the addon's registry rows.
//
// Secret inputs are create-only. Each write stores the inputs it carries in a
// new immutable Secret and points their valuesFrom entries at it — a change
// of spec, so the operator plans again. portal-api never reads, changes or
// deletes a Secret; the operator collects the ones nothing references. A
// request is checked in full, by portal-api and by the apiserver in a dry run,
// before any Secret is created, so a refused request stores nothing.
//
// portal-api never renders a chart, never applies a workload and never
// generates a value: the operator does all three.

// chartClient is the operator service as the chart addon endpoints use it.
// *operator.Service implements it.
type chartClient interface {
	Available() bool
	OperatorInfo(ctx context.Context) (operator.OperatorInfo, error)
	ChartAddons(ctx context.Context) ([]operator.ChartAddon, error)
	ChartAddon(ctx context.Context, name string) (*operator.ChartAddon, error)
	CreateChartAddon(ctx context.Context, a *operator.ChartAddon, dryRun bool) error
	UpdateChartAddon(ctx context.Context, a *operator.ChartAddon, dryRun bool) error
	DeleteChartAddon(ctx context.Context, name string) error
	SetKeepValues(ctx context.Context, name string, keep bool) error
	CreateValuesSecret(ctx context.Context, addon string, values map[string]string) (string, error)
}

// Chart addon limits.
const (
	maxAddonName    = 40      // a release name; Secrets and labels derive from it
	maxChartRef     = 2048    // a URL, generously
	maxSecretInputs = 64      // secret inputs of one addon, and of one request
	maxValuesPath   = 250     // the CRD's limit on valuesFrom[].targetPath
	maxSecretKey    = 253     // a key of a Secret
	maxChartBody    = 1 << 20 // the resource must fit etcd with room to spare
	updateAttempts  = 4       // read-modify-write retries on a conflict
)

var (
	// ociTag is what an OCI tag may be; Helm writes a version's '+' as '_',
	// and accepts either.
	ociTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]{0,127}$`)
	// chartDigest pins a chart archive.
	chartDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// archiveVersion is the "-1.2.0" a packaged chart's file name ends with.
	archiveVersion = regexp.MustCompile(`-v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$`)
	// valuesPathSegment is one segment of a dotted values path. Together with
	// the dots, a path is a valid Secret key.
	valuesPathSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// secretKeyChars is what a key of a Secret is made of.
	secretKeyChars = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	// secretName is a DNS-1123 subdomain: what a Secret may be called.
	secretName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// badInput is a request refused before anything is written.
type badInput struct{ msg string }

func (e *badInput) Error() string { return e.msg }

func bad(format string, args ...any) error { return &badInput{msg: fmt.Sprintf(format, args...)} }

// errUnchanged aborts an update that has nothing to write.
var errUnchanged = errors.New("unchanged")

// normaliseChart validates a chart reference as an admin typed it and returns
// it as a ZaentrumAddon carries it: an oci:// reference without a tag plus the
// version it needs, or an https:// link to a chart archive, which is one
// version and carries none.
func normaliseChart(ref, version, digest string) (operator.ChartSource, error) {
	ref, version, digest = strings.TrimSpace(ref), strings.TrimSpace(version), strings.TrimSpace(digest)
	switch {
	case ref == "":
		return operator.ChartSource{}, bad("chart is required: an oci:// reference or an https:// link to a chart archive")
	case len(ref) > maxChartRef:
		return operator.ChartSource{}, bad("chart reference is longer than %d characters", maxChartRef)
	case hasControl(ref) || strings.ContainsAny(ref, " \\"):
		return operator.ChartSource{}, bad("chart reference must not contain spaces, backslashes or control characters")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return operator.ChartSource{}, bad("chart %q is not a valid reference: %v", ref, err)
	}
	if u.User != nil {
		return operator.ChartSource{}, bad("chart reference must not carry credentials")
	}
	switch strings.ToLower(u.Scheme) {
	case "oci":
		repo := strings.Trim(u.Path, "/")
		if u.Host == "" || repo == "" || u.RawQuery != "" || u.Fragment != "" {
			return operator.ChartSource{}, bad("chart %q: an oci:// reference is a registry and a repository, e.g. oci://ghcr.io/example/charts/example", ref)
		}
		last := path.Base(repo)
		if strings.Contains(last, "@") {
			return operator.ChartSource{}, bad("chart %q: pin an oci:// chart with digest, not in the reference", ref)
		}
		if i := strings.LastIndexByte(last, ':'); i >= 0 {
			tag := last[i+1:]
			switch {
			case version == "":
				version = tag
			case version != tag:
				return operator.ChartSource{}, bad("version %q does not match the tag %q in the chart reference", version, tag)
			}
			repo = repo[:len(repo)-len(last)+i]
		}
		if version == "" {
			return operator.ChartSource{}, bad("version is required for an oci:// chart")
		}
		if !ociTag.MatchString(version) {
			return operator.ChartSource{}, bad("version %q is not a valid chart version", version)
		}
		ref = "oci://" + u.Host + "/" + repo
	case "https":
		if u.Host == "" || u.Fragment != "" {
			return operator.ChartSource{}, bad("chart %q: an https:// chart is a link to a chart archive", ref)
		}
		u.Scheme = "https"
		ref, version = u.String(), ""
	default:
		return operator.ChartSource{}, bad("chart must be an oci:// reference or an https:// link to a chart archive")
	}
	if digest != "" && !chartDigest.MatchString(digest) {
		return operator.ChartSource{}, bad("digest %q must be sha256: and 64 lower-case hex digits", digest)
	}
	return operator.ChartSource{Ref: ref, Version: version, Digest: digest}, nil
}

// ociTagOf is the tag an oci:// reference carries in its last segment; "".
func ociTagOf(ref string) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref)), "oci://") {
		return ""
	}
	last := path.Base(strings.TrimRight(strings.TrimSpace(ref), "/"))
	if i := strings.LastIndexByte(last, ':'); i >= 0 {
		return last[i+1:]
	}
	return ""
}

// defaultAddonName is an addon's name when the admin gives none: the last
// path segment of its chart reference without a tag, a version or an archive
// extension — oci://…/charts/example:1.2.0 and https://…/example-1.2.0.tgz
// are both "example".
func defaultAddonName(ref string) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	last := path.Base(strings.TrimRight(u.Path, "/"))
	if last == "." || last == "/" {
		return ""
	}
	if i := strings.IndexAny(last, ":@"); i >= 0 {
		last = last[:i]
	}
	for _, ext := range []string{".tar.gz", ".tgz"} {
		if strings.HasSuffix(strings.ToLower(last), ext) {
			last = last[:len(last)-len(ext)]
			break
		}
	}
	return strings.ToLower(archiveVersion.ReplaceAllString(last, ""))
}

// validAddonName: an addon's name is its release name and its registry key.
func validAddonName(name string) error {
	if !isDNSLabel(name) || len(name) > maxAddonName {
		return bad("name %q must be a DNS-1123 label of at most %d characters: lower-case letters, digits and '-'", name, maxAddonName)
	}
	return nil
}

// parseValues accepts non-secret values: absent or null for none, otherwise a
// JSON object that leaves the reserved top-level key "zaentrum" alone.
func parseValues(raw json.RawMessage) (json.RawMessage, error) {
	if isNullJSON(raw) {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, bad("values must be a JSON object")
	}
	if _, ok := obj["zaentrum"]; ok {
		return nil, bad(`values: the top-level key "zaentrum" is reserved for the platform's values`)
	}
	if len(obj) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, bad("values must be a JSON object")
	}
	return buf.Bytes(), nil
}

func isNullJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

// validValuesPath: a secret input is addressed by a dotted values path — the
// entry's targetPath, and its key in the Secret the input is written to.
func validValuesPath(p string) error {
	if p == "" || len(p) > maxValuesPath {
		return bad("secret input %q: a dotted values path of at most %d characters", p, maxValuesPath)
	}
	for _, seg := range strings.Split(p, ".") {
		if !valuesPathSegment.MatchString(seg) {
			return bad("secret input %q: a dotted values path whose segments are letters, digits, '-' and '_'", p)
		}
	}
	if p == "zaentrum" || strings.HasPrefix(p, "zaentrum.") {
		return bad("secret input %q: zaentrum is reserved for the platform's values", p)
	}
	return nil
}

// secretRefInput is a request's reference to a Secret the addon kept: a key of
// a Secret whose name says it belongs to the addon.
type secretRefInput struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// inputChanges is what a request does to an addon's secret inputs: values to
// store in a new Secret, references to Secrets that exist, paths to clear.
type inputChanges struct {
	set   map[string]string
	refs  map[string]operator.SecretRef
	clear []string
}

// parseInputChanges checks a request's secret inputs for an addon. A path is
// set, referenced or cleared — never two of these at once.
func parseInputChanges(addon string, set map[string]string, refs map[string]secretRefInput, clear []string) (inputChanges, error) {
	c := inputChanges{set: set, refs: map[string]operator.SecretRef{}, clear: clear}
	if len(set)+len(refs)+len(clear) > maxSecretInputs {
		return c, bad("at most %d secret inputs in one request", maxSecretInputs)
	}
	touched := map[string]string{}
	claim := func(p, how string) error {
		if err := validValuesPath(p); err != nil {
			return err
		}
		if before, ok := touched[p]; ok {
			return bad("secret input %q is both %s and %s", p, before, how)
		}
		touched[p] = how
		return nil
	}
	for _, p := range sortedKeys(set) {
		if err := claim(p, "set"); err != nil {
			return c, err
		}
		if set[p] == "" {
			return c, bad("secret input %q is empty — clear it with clearSecrets instead", p)
		}
	}
	prefix := operator.AddonSecretPrefix(addon)
	for _, p := range sortedKeys(refs) {
		if err := claim(p, "referenced"); err != nil {
			return c, err
		}
		name, key := strings.TrimSpace(refs[p].Name), strings.TrimSpace(refs[p].Key)
		if key == "" {
			key = p
		}
		switch {
		case !strings.HasPrefix(name, prefix) || len(name) > 253 || !secretName.MatchString(name):
			return c, bad("secret input %q: a reference names a Secret of the addon, %s…", p, prefix)
		case len(key) > maxSecretKey || !secretKeyChars.MatchString(key):
			return c, bad("secret input %q: %q is no key of a Secret", p, key)
		}
		c.refs[p] = operator.SecretRef{Name: name, Key: key}
	}
	for _, p := range clear {
		if err := claim(p, "cleared"); err != nil {
			return c, err
		}
	}
	return c, nil
}

// apply points an addon's secret inputs where the changes say; secret is the
// Secret the set values are stored in.
func (c inputChanges) apply(ca *operator.ChartAddon, secret string) error {
	for _, p := range sortedKeys(c.refs) {
		ca.SetSecretInput(p, c.refs[p])
	}
	for _, p := range sortedKeys(c.set) {
		ca.SetSecretInput(p, operator.SecretRef{Name: secret, Key: p})
	}
	for _, p := range c.clear {
		ca.ClearSecretInput(p)
	}
	if n := len(ca.SecretInputs()); n > maxSecretInputs {
		return bad("at most %d secret inputs; the addon would have %d", maxSecretInputs, n)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pendingSecret stands in for the name of the Secret a write stores its secret
// inputs in, in the dry run that validates the write before that Secret exists.
func pendingSecret(addon string) string { return operator.ValuesSecretPrefix(addon) + "pending" }

// orphanedSecret is a write that failed after its secret inputs were stored:
// the Secret exists, and nothing references it.
type orphanedSecret struct {
	err    error
	secret string
}

func (e *orphanedSecret) Error() string {
	return e.err.Error() + " — the secret inputs had been stored in Secret " + e.secret +
		", which nothing references; the operator removes it"
}

func (e *orphanedSecret) Unwrap() error { return e.err }

// orphaned says, with err, that secret was left behind; err alone without one.
func orphaned(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	return &orphanedSecret{err: err, secret: secret}
}

// installable answers whether an addon's plan permits installing it: the
// operator planned the current generation, and found nothing to refuse and
// no values error.
func installable(ca *operator.ChartAddon) error {
	switch {
	case ca.Plan == nil || ca.ObservedGeneration != ca.Generation:
		msg := "the addon has no plan for its current configuration yet — wait for the operator to plan it"
		if ca.Message != "" && ca.ObservedGeneration == ca.Generation {
			msg += ": " + ca.Message
		}
		return &conflictError{msg: msg}
	case len(ca.Plan.Violations) > 0:
		return &conflictError{msg: "the plan refuses the chart: " + strings.Join(ca.Plan.Violations, "; ")}
	case len(ca.Plan.ValuesErrors) > 0:
		return &conflictError{msg: "the plan has values errors: " + strings.Join(ca.Plan.ValuesErrors, "; ")}
	}
	return nil
}

// ─── cluster answers ─────────────────────────────────────────────────────────

const noClusterNote = "installing addons from charts needs portal-api to run in a cluster"

// chartsNote says why chart addons cannot be managed here, or "" when they can.
func (a *API) chartsNote(ctx context.Context) string {
	if a.charts == nil || !a.charts.Available() {
		return noClusterNote
	}
	switch _, err := a.charts.ChartAddons(ctx); {
	case err == nil:
		return ""
	case k8s.IsNotFound(err):
		return "this cluster serves no ZaentrumAddon resource — update the zaentrum-operator to install addons from charts"
	case k8s.IsForbidden(err):
		return "portal-api may not manage ZaentrumAddon resources — update the zaentrum-operator, whose portal-api Role grants it"
	default:
		return "the cluster did not answer: " + err.Error()
	}
}

// chartsReady guards the chart endpoints outside a cluster.
func (a *API) chartsReady(w http.ResponseWriter) bool {
	if a.charts == nil || !a.charts.Available() {
		http.Error(w, noClusterNote, http.StatusServiceUnavailable)
		return false
	}
	return true
}

// writeChartError answers a refused request or a failed cluster call.
func writeChartError(w http.ResponseWriter, err error) {
	var (
		bi *badInput
		ce *conflictError
		ae *k8s.APIError
	)
	switch {
	case errors.As(err, &bi):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &ce):
		http.Error(w, err.Error(), http.StatusConflict)
	case k8s.IsForbidden(err):
		http.Error(w, "portal-api is not allowed to do this in the cluster — update the zaentrum-operator, whose portal-api Role grants it: "+err.Error(),
			http.StatusServiceUnavailable)
	case k8s.IsConflict(err):
		http.Error(w, "the addon changed while this request was applied — read it again and retry: "+err.Error(), http.StatusConflict)
	case errors.As(err, &ae) && (ae.Code == http.StatusBadRequest || ae.Code == http.StatusUnprocessableEntity):
		http.Error(w, "the cluster refused the addon: "+err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, "the cluster did not answer as expected: "+err.Error(), http.StatusBadGateway)
	}
}

// chartMissing answers a NotFound for one addon: there is no such addon, or
// the cluster serves no ZaentrumAddons at all.
func (a *API) chartMissing(ctx context.Context, w http.ResponseWriter, name string) {
	if note := a.chartsNote(ctx); note != "" {
		http.Error(w, note, http.StatusServiceUnavailable)
		return
	}
	http.Error(w, fmt.Sprintf("no addon %q is installed from a chart", name), http.StatusNotFound)
}

// updateChartAddon reads an addon, applies change, writes it back and returns
// it as written. A write that lost a race — with an admin, or with the
// operator's status — is redone from a fresh read, so change must be safe to
// apply again. change returns errUnchanged when there is nothing to write; the
// addon is then returned as read.
func (a *API) updateChartAddon(ctx context.Context, name string, change func(*operator.ChartAddon) error) (*operator.ChartAddon, error) {
	for attempt := 1; ; attempt++ {
		ca, err := a.charts.ChartAddon(ctx, name)
		if err != nil {
			return nil, err
		}
		if err := change(ca); errors.Is(err, errUnchanged) {
			return ca, nil
		} else if err != nil {
			return nil, err
		}
		err = a.charts.UpdateChartAddon(ctx, ca, false)
		if err == nil {
			return ca, nil
		}
		if !k8s.IsConflict(err) || attempt == updateAttempts {
			return nil, err
		}
	}
}

// chartAccepted answers a write: the addon's name, and what a client needs
// to tell the plan for this write from an older one — the generation it made
// and the one the operator has planned so far — with the chart it names.
type chartAccepted struct {
	Name               string               `json:"name"`
	Generation         int64                `json:"generation"`
	ObservedGeneration int64                `json:"observedGeneration"`
	Chart              operator.ChartSource `json:"chart"`
}

func accepted(ca *operator.ChartAddon) chartAccepted {
	return chartAccepted{Name: ca.Name, Generation: ca.Generation, ObservedGeneration: ca.ObservedGeneration, Chart: ca.Chart}
}

// chartNameTaken refuses a new chart addon whose name the registry already
// gives to something else — an addon added by address, or an app registered
// by hand: registering the chart would take it over.
func (a *API) chartNameTaken(ctx context.Context, name string) error {
	switch ad, err := a.addons.GetAddon(ctx, name); {
	case err == nil && ad.ChartRef == "":
		from := ad.Address
		if from == "" {
			from = "an address"
		}
		return &conflictError{msg: fmt.Sprintf("an addon %q is already installed from %s — remove it first, or choose another name", name, from)}
	case err == nil:
		return nil // registered from a chart whose resource is gone: adding it again takes its rows back
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	switch _, err := a.addons.GetApp(ctx, name); {
	case err == nil:
		return &conflictError{msg: fmt.Sprintf("an app with key %q is already registered — remove or rename it first, or choose another name", name)}
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	return nil
}

// ─── handlers ────────────────────────────────────────────────────────────────

// addonChartsStatus handles GET /api/portal/addon-charts: whether addons can
// be installed from charts here, and if not, why. The settings console asks
// before it offers to.
func (a *API) addonChartsStatus(w http.ResponseWriter, r *http.Request) {
	note := a.chartsNote(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"available": note == "", "note": note})
}

// createAddonChart handles POST /api/portal/addon-charts
// {name?, chart, version?, digest?, values?, secretValues?, secretRefs?}.
//
// It creates the ZaentrumAddon — or updates the one of that name — suspended,
// so the operator plans it and applies nothing. Values are replaced; secret
// inputs are added to those already set, because a client cannot send back
// what it can never read. secretRefs point inputs at Secrets the addon kept
// when it was removed before.
func (a *API) createAddonChart(w http.ResponseWriter, r *http.Request) {
	if !a.chartsReady(w) {
		return
	}
	var body struct {
		Name         string                    `json:"name"`
		Chart        string                    `json:"chart"`
		Version      string                    `json:"version"`
		Digest       string                    `json:"digest"`
		Values       json.RawMessage           `json:"values"`
		SecretValues map[string]string         `json:"secretValues"`
		SecretRefs   map[string]secretRefInput `json:"secretRefs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChartBody)).Decode(&body); err != nil {
		badRequest(w, "invalid json: "+err.Error())
		return
	}
	chart, err := normaliseChart(body.Chart, body.Version, body.Digest)
	if err != nil {
		writeChartError(w, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = defaultAddonName(chart.Ref)
	}
	values, err := parseValues(body.Values)
	if err == nil {
		err = validAddonName(name)
	}
	var inputs inputChanges
	if err == nil {
		inputs, err = parseInputChanges(name, body.SecretValues, body.SecretRefs, nil)
	}
	if err != nil {
		writeChartError(w, err)
		return
	}
	a.noteOrigin(r)
	ctx := r.Context()
	a.registration.mu.Lock()
	defer a.registration.mu.Unlock()

	existing, err := a.charts.ChartAddon(ctx, name)
	switch {
	case k8s.IsNotFound(err):
		existing = nil
		if note := a.chartsNote(ctx); note != "" {
			http.Error(w, note, http.StatusServiceUnavailable)
			return
		}
		if err := a.chartNameTaken(ctx, name); err != nil {
			writeChartError(w, err)
			return
		}
	case err != nil:
		writeChartError(w, err)
		return
	}
	change := func(ca *operator.ChartAddon, secret string) error {
		ca.Chart, ca.Values, ca.Suspend = chart, values, true
		return inputs.apply(ca, secret)
	}
	fresh := func(secret string) (*operator.ChartAddon, error) {
		ca := &operator.ChartAddon{Name: name}
		return ca, change(ca, secret)
	}
	// Checked in full — here, and by the apiserver in a dry run — before a
	// secret input is stored: a refused request leaves no Secret behind.
	secret := ""
	if len(inputs.set) > 0 {
		var candidate *operator.ChartAddon
		if existing == nil {
			if candidate, err = fresh(pendingSecret(name)); err == nil {
				err = a.charts.CreateChartAddon(ctx, candidate, true)
			}
		} else if err = change(existing, pendingSecret(name)); err == nil {
			err = a.charts.UpdateChartAddon(ctx, existing, true)
		}
		if err == nil {
			secret, err = a.charts.CreateValuesSecret(ctx, name, inputs.set)
		}
		if err != nil {
			writeChartError(w, err)
			return
		}
	}
	var written *operator.ChartAddon
	if existing == nil {
		if written, err = fresh(secret); err == nil {
			err = a.charts.CreateChartAddon(ctx, written, false)
		}
		if k8s.IsConflict(err) {
			err = &conflictError{msg: fmt.Sprintf("an addon %q was created meanwhile — read it and try again", name)}
		}
	} else {
		written, err = a.updateChartAddon(ctx, name, func(ca *operator.ChartAddon) error { return change(ca, secret) })
	}
	if err != nil {
		writeChartError(w, orphaned(err, secret))
		return
	}
	writeJSON(w, http.StatusAccepted, accepted(written))
}

// chartAddonView is GET /api/portal/addon-charts/{name}: the resource as the
// settings console and zae show it. No secret value — secretKeys names the
// inputs that are set and secretRefs where each is read from, both taken from
// the resource's valuesFrom.
type chartAddonView struct {
	Name               string                        `json:"name"`
	Chart              operator.ChartSource          `json:"chart"`
	Suspended          bool                          `json:"suspended"`
	Phase              string                        `json:"phase"`
	Message            string                        `json:"message"`
	Generation         int64                         `json:"generation"`
	ObservedGeneration int64                         `json:"observedGeneration"`
	Plan               json.RawMessage               `json:"plan"`
	Components         []operator.ChartComponent     `json:"components"`
	LastAppliedChart   *operator.ChartSource         `json:"lastAppliedChart"`
	Values             json.RawMessage               `json:"values"`
	SecretKeys         []string                      `json:"secretKeys"`
	SecretRefs         map[string]operator.SecretRef `json:"secretRefs"`
	// Registered: the registry holds the addon, registered from its chart.
	Registered        bool   `json:"registered"`
	RegistrationError string `json:"registrationError,omitempty"`
}

// getAddonChart handles GET /api/portal/addon-charts/{name}.
func (a *API) getAddonChart(w http.ResponseWriter, r *http.Request) {
	if !a.chartsReady(w) {
		return
	}
	name := chi.URLParam(r, "name")
	if err := validAddonName(name); err != nil {
		writeChartError(w, err)
		return
	}
	ctx := r.Context()
	ca, err := a.charts.ChartAddon(ctx, name)
	if k8s.IsNotFound(err) {
		a.chartMissing(ctx, w, name)
		return
	}
	if err != nil {
		writeChartError(w, err)
		return
	}
	inputs := ca.SecretInputs()
	view := chartAddonView{
		Name: name, Chart: ca.Chart, Suspended: ca.Suspend, Phase: ca.Phase, Message: ca.Message,
		Generation: ca.Generation, ObservedGeneration: ca.ObservedGeneration,
		Plan: ca.PlanRaw, Components: nonNil(ca.Components), LastAppliedChart: ca.LastAppliedChart,
		Values: ca.Values, SecretKeys: sortedKeys(inputs), SecretRefs: inputs,
		RegistrationError: a.registrationError(name),
	}
	if ad, err := a.addons.GetAddon(ctx, name); err == nil && ad.ChartRef != "" {
		view.Registered = true
	}
	if ca.Phase == operator.ChartPhaseReady && !view.Registered {
		a.kickRegistration() // ready and waiting: do not make the admin wait for the next tick
	}
	writeJSON(w, http.StatusOK, view)
}

// installAddonChart handles POST /api/portal/addon-charts/{name}/install:
// suspend off, so the operator applies the plan. Refused with 409 while the
// current generation has no plan, or its plan has violations or values errors.
func (a *API) installAddonChart(w http.ResponseWriter, r *http.Request) {
	if !a.chartsReady(w) {
		return
	}
	name := chi.URLParam(r, "name")
	if err := validAddonName(name); err != nil {
		writeChartError(w, err)
		return
	}
	a.noteOrigin(r)
	ctx := r.Context()
	written, err := a.updateChartAddon(ctx, name, func(ca *operator.ChartAddon) error {
		if err := installable(ca); err != nil {
			return err
		}
		if !ca.Suspend {
			return errUnchanged // installing already
		}
		ca.Suspend = false
		return nil
	})
	if k8s.IsNotFound(err) {
		a.chartMissing(ctx, w, name)
		return
	}
	if err != nil {
		writeChartError(w, err)
		return
	}
	a.kickRegistration()
	writeJSON(w, http.StatusAccepted, accepted(written))
}

// patchAddonChart handles PATCH /api/portal/addon-charts/{name}
// {chart?, version?, digest?, values?, secretValues?, secretRefs?,
// clearSecrets?, suspend?}: upgrade or reconfigure.
//
// A new chart, version or digest suspends the addon — the operator plans it
// and the admin installs the plan — unless suspend: false is sent. Values are
// replaced when present (null removes them). Only the secret inputs the request
// names are touched: every other valuesFrom entry keeps its place and fields.
func (a *API) patchAddonChart(w http.ResponseWriter, r *http.Request) {
	if !a.chartsReady(w) {
		return
	}
	name := chi.URLParam(r, "name")
	if err := validAddonName(name); err != nil {
		writeChartError(w, err)
		return
	}
	var body struct {
		Chart        *string                   `json:"chart"`
		Version      *string                   `json:"version"`
		Digest       *string                   `json:"digest"`
		Values       json.RawMessage           `json:"values"` // absent: unchanged; null: none
		SecretValues map[string]string         `json:"secretValues"`
		SecretRefs   map[string]secretRefInput `json:"secretRefs"`
		ClearSecrets []string                  `json:"clearSecrets"`
		Suspend      *bool                     `json:"suspend"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChartBody)).Decode(&body); err != nil {
		badRequest(w, "invalid json: "+err.Error())
		return
	}
	var (
		values json.RawMessage
		err    error
	)
	if body.Values != nil {
		values, err = parseValues(body.Values)
	}
	var inputs inputChanges
	if err == nil {
		inputs, err = parseInputChanges(name, body.SecretValues, body.SecretRefs, body.ClearSecrets)
	}
	if err != nil {
		writeChartError(w, err)
		return
	}
	a.noteOrigin(r)
	ctx := r.Context()
	a.registration.mu.Lock()
	defer a.registration.mu.Unlock()

	current, err := a.charts.ChartAddon(ctx, name)
	if k8s.IsNotFound(err) {
		a.chartMissing(ctx, w, name)
		return
	} else if err != nil {
		writeChartError(w, err)
		return
	}
	change := func(ca *operator.ChartAddon, secret string) error {
		changed := false
		if body.Chart != nil || body.Version != nil || body.Digest != nil {
			ref, version, digest := ca.Chart.Ref, ca.Chart.Version, ca.Chart.Digest
			if body.Chart != nil {
				ref = *body.Chart
				if body.Version == nil && ociTagOf(ref) != "" {
					version = "" // the new reference says which version
				}
			}
			if body.Version != nil {
				version = *body.Version
			}
			if body.Digest != nil {
				digest = *body.Digest
			}
			chart, err := normaliseChart(ref, version, digest)
			if err != nil {
				return err
			}
			if body.Digest == nil && (chart.Ref != ca.Chart.Ref || chart.Version != ca.Chart.Version) {
				// A digest pins one archive; another chart or version is not it.
				chart.Digest = ""
			}
			changed = chart != ca.Chart
			ca.Chart = chart
		}
		if body.Values != nil {
			ca.Values = values
		}
		if err := inputs.apply(ca, secret); err != nil {
			return err
		}
		switch {
		case body.Suspend != nil:
			ca.Suspend = *body.Suspend
		case changed:
			ca.Suspend = true // plan the new chart first
		}
		return nil
	}
	// Checked in full — here, and by the apiserver in a dry run — before a
	// secret input is stored: a refused request leaves no Secret behind.
	if err := change(current, pendingSecret(name)); err != nil {
		writeChartError(w, err)
		return
	}
	secret := ""
	if len(inputs.set) > 0 {
		err := a.charts.UpdateChartAddon(ctx, current, true)
		if err == nil {
			secret, err = a.charts.CreateValuesSecret(ctx, name, inputs.set)
		}
		if err != nil {
			writeChartError(w, err)
			return
		}
	}
	written, err := a.updateChartAddon(ctx, name, func(ca *operator.ChartAddon) error { return change(ca, secret) })
	if err != nil {
		writeChartError(w, orphaned(err, secret))
		return
	}
	if body.Suspend != nil && !*body.Suspend {
		a.kickRegistration()
	}
	writeJSON(w, http.StatusAccepted, accepted(written))
}

// removeAddonChart handles DELETE /api/portal/addon-charts/{name}?keepValues=.
//
// keepValues first annotates the ZaentrumAddon keep-values; the operator's
// finalizer then keeps the Secrets with its secret inputs and generated values
// when the resource goes. Then the ZaentrumAddon is deleted — the operator and
// garbage collection take the workloads and everything else it owns — and so
// are the addon's registry rows. portal-api touches no Secret. An addon that
// does not exist is answered 404 before anything happens; a plan-only addon is
// cancelled the same way.
func (a *API) removeAddonChart(w http.ResponseWriter, r *http.Request) {
	if !a.chartsReady(w) {
		return
	}
	name := chi.URLParam(r, "name")
	if err := validAddonName(name); err != nil {
		writeChartError(w, err)
		return
	}
	keep := false
	if v := strings.TrimSpace(r.URL.Query().Get("keepValues")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			badRequest(w, "keepValues must be true or false")
			return
		}
		keep = b
	}
	ctx := r.Context()
	a.registration.mu.Lock()
	defer a.registration.mu.Unlock()

	ca, err := a.charts.ChartAddon(ctx, name)
	if k8s.IsNotFound(err) {
		a.chartMissing(ctx, w, name)
		return
	} else if err != nil {
		writeChartError(w, err)
		return
	}
	// The annotation is the operator's instruction when the resource goes: set
	// it first — and take away one an earlier attempt left.
	if keep != ca.KeepValues {
		if err := a.charts.SetKeepValues(ctx, name, keep); err != nil {
			writeChartError(w, err)
			return
		}
	}
	if err := a.charts.DeleteChartAddon(ctx, name); err != nil && !k8s.IsNotFound(err) {
		writeChartError(w, err)
		return
	}
	removed, err := a.removeChartRegistration(ctx, name)
	if err != nil {
		serverError(w, err)
		return
	}
	out := map[string]any{"name": name, "resource": true, "keptValues": keep}
	if removed != nil {
		out["removed"] = map[string]any{
			"tiles": removed.Tiles, "rows": removed.Rows, "space": strings.Join(removed.Spaces, ","),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// removeChartRegistration deletes what the registry holds for a chart addon —
// never an addon of the same key added by address, which is removed as such.
// nil when there was nothing.
func (a *API) removeChartRegistration(ctx context.Context, name string) (*store.AddonRemoval, error) {
	a.forgetRegistration(name)
	ad, err := a.addons.GetAddon(ctx, name)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ad.ChartRef == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	declaredSpace := ""
	if d, ok := storedManifest(*ad); ok && d.UI != nil && d.UI.Space != nil {
		declaredSpace = strings.TrimSpace(d.UI.Space.Key)
	}
	removed, err := a.addons.RemoveAddon(ctx, name, declaredSpace)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	invalidateDiscovery()
	return &removed, nil
}

// ─── registration state ──────────────────────────────────────────────────────

// chartRegistration is the registration loop's state. The zero value works:
// an API built without New has no loop to kick.
type chartRegistration struct {
	// mu serialises writes for chart addons — create, patch, remove and the
	// loop's registry writes — so a removal and a registration never cross.
	mu sync.Mutex
	// kick wakes the loop before its next tick; nil when no loop runs.
	kick chan struct{}

	stateMu   sync.Mutex
	errors    map[string]string // addon → why its registration failed last
	listError string            // why the last pass could not list the addons
	origin    string            // the portal's public origin, as last requested

	// fetch reads a manifest; fetchManifest unless a test substitutes one.
	fetch func(ctx context.Context, proxyURL string) (Descriptor, error)
}

// kickRegistration wakes the registration loop, if one runs; never blocks.
func (a *API) kickRegistration() {
	select {
	case a.registration.kick <- struct{}{}:
	default:
	}
}

func (a *API) registrationError(name string) string {
	a.registration.stateMu.Lock()
	defer a.registration.stateMu.Unlock()
	return a.registration.errors[name]
}

func (a *API) forgetRegistration(name string) {
	a.registration.stateMu.Lock()
	defer a.registration.stateMu.Unlock()
	delete(a.registration.errors, name)
}

// noteOrigin remembers the public origin an admin reached the portal on: the
// registration loop, which has no request, absolutises slot URLs with it when
// the platform does not name its hostname.
func (a *API) noteOrigin(r *http.Request) {
	if origin := requestOrigin(r); origin != "" {
		a.registration.stateMu.Lock()
		a.registration.origin = origin
		a.registration.stateMu.Unlock()
	}
}
