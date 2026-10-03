// Package api exposes the portal registry over REST. The launchpad + identity
// reads are available to any signed-in user; apps/spaces/tiles writes are gated
// on the realm admin role by the router (see auth.Middleware.RequireAdmin).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/dbbrowse"
	"github.com/zaentrum/zaentrum-portal/server/internal/eventtap"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/redact"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

type API struct {
	reg    registryStore
	addons addonStore
	cfg    config.Config
	op     *operator.Service
	tap    *eventtap.Tap
	br     *dbbrowse.Browser
	// workloads is op as the addon endpoints read it; nil when op is.
	workloads workloadSource
	// charts is op as the chart addon endpoints use it; nil when op is.
	charts chartClient
	// registration is the chart addon registration loop's state.
	registration chartRegistration
}

// addonStore is the part of the registry the addon endpoints and capability
// discovery use. *store.Store implements it; tests substitute an in-memory one.
type addonStore interface {
	ListApps(ctx context.Context) ([]model.App, error)
	GetApp(ctx context.Context, key string) (*model.App, error)
	ListSpaces(ctx context.Context) ([]model.Space, error)
	ListAddons(ctx context.Context) ([]model.Addon, error)
	GetAddon(ctx context.Context, key string) (*model.Addon, error)
	WorkloadClaims(ctx context.Context) (map[string]string, error)
	InstallAddon(ctx context.Context, in store.AddonInstall) error
	RemoveAddon(ctx context.Context, key, declaredSpace string) (store.AddonRemoval, error)
	SetRegistrationError(ctx context.Context, name, msg string) error
	RegistrationErrors(ctx context.Context) (map[string]string, error)
}

// registryStore is the registry as the launchpad, the registry console, the
// slot API and the app proxy use it. *store.Store implements it; tests
// substitute an in-memory one, so the rules those handlers keep are tested
// without a database.
type registryStore interface {
	Launchpad(ctx context.Context, roles []string) (model.Launchpad, error)
	ListApps(ctx context.Context) ([]model.App, error)
	GetApp(ctx context.Context, key string) (*model.App, error)
	UpsertApp(ctx context.Context, app model.App) error
	DeleteApp(ctx context.Context, key string) error
	ListSpaces(ctx context.Context) ([]model.Space, error)
	GetSpace(ctx context.Context, key string) (*model.Space, error)
	UpsertSpace(ctx context.Context, sp model.Space) error
	DeleteSpace(ctx context.Context, key string) error
	ListTiles(ctx context.Context) ([]model.Tile, error)
	GetTile(ctx context.Context, key string) (*model.Tile, error)
	UpsertTile(ctx context.Context, t model.Tile) error
	DeleteTile(ctx context.Context, key string) error
	ListExtensions(ctx context.Context) ([]model.Extension, error)
	ListExtensionsForSlot(ctx context.Context, slot string) ([]model.Extension, error)
	GetExtension(ctx context.Context, key string) (*model.Extension, error)
	UpsertExtension(ctx context.Context, e model.Extension) error
	DeleteExtension(ctx context.Context, key string) error
}

func New(st *store.Store, cfg config.Config, op *operator.Service, tap *eventtap.Tap, br *dbbrowse.Browser) *API {
	a := &API{reg: st, addons: st, cfg: cfg, op: op, tap: tap, br: br}
	a.registration.kick = make(chan struct{}, 1)
	if op != nil {
		a.workloads = op // never a typed nil inside the interface
		a.charts = op
	}
	return a
}

// Register mounts the registry routes under /api/portal. Authn is applied here
// per-route: an embedded app's public bundle and CLI discovery are open, and
// everything else requires a signed-in user. Admin writes are additionally
// gated via mw.RequireAdmin.
func (a *API) Register(r chi.Router, mw *auth.Middleware) {
	r.Route("/api/portal", func(r chi.Router) {
		// Embedded apps: the shell hosts an app in its own page and everything
		// that app loads — its module bundle and its API — comes back through
		// here, so the portal stays the single front door. Its public bundle
		// is open — the browser fetches it with a plain import() and <link>,
		// which carry no bearer — and everything else it serves takes a
		// signed-in user (appproxy.go). The SSRF guard limits the destination
		// to a registered in-cluster app.
		r.Handle("/apps/{key}/*", a.proxyAuth(mw, http.HandlerFunc(a.appProxy)))

		// CLI capability discovery — also deliberately unauthenticated: the
		// zae CLI probes it before any login flow exists, and it aggregates
		// route METADATA of an open-source platform, not data. See
		// clidiscovery.go for the SSRF reasoning (candidates come only from
		// the platform's own registries, never from the request).
		r.Get("/cli/discovery", a.cliDiscovery)

		// Everything below needs a signed-in user.
		r.Group(func(r chi.Router) {
			r.Use(mw.Authn)

			// Reads for any signed-in user.
			r.Get("/launchpad", a.launchpad)
			r.Get("/me", a.me)
			// Product apps read the enabled extension contributions for a slot
			// (chino forwards the user's bearer here). Any signed-in user.
			r.Get("/slots/{slot}", a.slotExtensions)

			// Registry administration (settings console).
			r.Group(func(ar chi.Router) {
				ar.Use(mw.RequireAdmin)

				// Addon installation: pull the addon's manifest and materialise
				// its app, tiles, slot rows and component group — see addons.go.
				ar.Get("/addons", a.listAddons)
				ar.Post("/addons", a.installAddon)
				ar.Delete("/addons/{key}", a.removeAddon)

				// Addons as Helm charts: portal-api writes the ZaentrumAddon
				// and its values Secret; the operator plans and installs the
				// chart — see addoncharts.go.
				ar.Get("/addon-charts", a.addonChartsStatus)
				ar.Post("/addon-charts", a.createAddonChart)
				ar.Get("/addon-charts/{name}", a.getAddonChart)
				ar.Patch("/addon-charts/{name}", a.patchAddonChart)
				ar.Delete("/addon-charts/{name}", a.removeAddonChart)
				ar.Post("/addon-charts/{name}/install", a.installAddonChart)

				ar.Get("/apps", a.listApps)
				ar.Post("/apps", a.upsertApp)
				ar.Patch("/apps/{key}", a.patchApp)
				ar.Delete("/apps/{key}", a.deleteApp)

				ar.Get("/spaces", a.listSpaces)
				ar.Post("/spaces", a.upsertSpace)
				ar.Patch("/spaces/{key}", a.patchSpace)
				ar.Delete("/spaces/{key}", a.deleteSpace)

				ar.Get("/tiles", a.listTiles)
				ar.Post("/tiles", a.upsertTile)
				ar.Patch("/tiles/{key}", a.patchTile)
				ar.Delete("/tiles/{key}", a.deleteTile)

				// Operator / instances console (view running services, scale, update,
				// monitor). Admin-only — it manages the platform's Deployments / CR.
				ar.Get("/operator", a.operatorGet)
				ar.Patch("/operator", a.operatorPatch)
				ar.Post("/operator/apply-update", a.operatorApplyUpdate)
				// Ask the operator to verify the platform now (verification.go
				// in the operator package): one annotation, the operator runs it.
				ar.Post("/operator/verify", a.operatorVerify)
				ar.Post("/operator/instances/{name}/scale", a.instanceScale)
				ar.Post("/operator/instances/{name}/restart", a.instanceRestart)

				// Debug: container logs (secrets redacted). Admin-only, read-only.
				ar.Get("/debug/pods", a.debugPods)
				ar.Get("/debug/logs", a.debugLogs)

				// Debug: Kafka event tap — live topology + recent events (redacted).
				ar.Get("/debug/kafka/topology", a.kafkaTopology)
				ar.Get("/debug/kafka/events", a.kafkaEvents)

				// Debug: curated read-only DB browser (whitelisted tables, masked).
				ar.Get("/debug/db/tables", a.dbTables)
				ar.Get("/debug/db/rows", a.dbRows)

				// Debug: downloadable support bundle (all sections secret-scrubbed).
				ar.Get("/debug/support-bundle", a.supportBundle)
			})

			// UI extension registry — writable by a human admin OR an addon's
			// service account (so an addon self-registers its own seam on install).
			r.Group(func(er chi.Router) {
				er.Use(mw.RequireAdminOrAddon)
				er.Get("/extensions", a.listExtensions)
				er.Post("/extensions", a.upsertExtension)
				er.Patch("/extensions/{key}", a.patchExtension)
				er.Delete("/extensions/{key}", a.deleteExtension)
			})
		})
	})
}

// ─── operator / instances ────────────────────────────────────────────────────

// operatorGet returns the whole console state in one call: whether instance
// management is available (in-cluster), the operator (Zaentrum CR) summary, and the
// live instances.
func (a *API) operatorGet(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"available": false, "operator": map[string]any{"present": false}, "instances": []any{}}
	if a.op == nil || !a.op.Available() {
		out["operator"] = map[string]any{"present": false, "note": "instance management is unavailable (not running in a cluster)"}
		writeJSON(w, http.StatusOK, out)
		return
	}
	info, _ := a.op.OperatorInfo(r.Context())
	instances, err := a.op.Instances(r.Context())
	if err != nil {
		out["available"] = true
		out["operator"] = info
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "operator": info, "instances": instances})
}

// The four writes below answer with what the write produced — a generation —
// rather than 204. A client that changes the platform and then waits has
// nothing else to wait on: the replica counters describe whatever pods exist,
// including the ones from before the write, so a wait keyed on them reports
// success while the rollout has not started. 204 is kept nowhere; the SPA
// ignores the body either way, and an older client that expects no body reads
// one it does not look at.

func (a *API) operatorPatch(w http.ResponseWriter, r *http.Request) {
	if !a.operatorReady(w) {
		return
	}
	var body struct {
		Version    *string `json:"version"`
		Channel    *string `json:"channel"`
		UpdateMode *string `json:"updateMode"`
	}
	if !decode(w, r, &body) {
		return
	}
	out, err := a.op.SetOperator(r.Context(), body.Version, body.Channel, body.UpdateMode)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) operatorApplyUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.operatorReady(w) {
		return
	}
	// The body is optional: version names the update the caller decided on, so
	// that applying one the operator has since replaced is refused instead of
	// rolling the platform to a version nobody chose.
	var body struct {
		Version string `json:"version"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	out, err := a.op.ApplyUpdate(r.Context(), strings.TrimSpace(body.Version))
	switch {
	case errors.Is(err, operator.ErrUpdateChanged):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		badRequest(w, err.Error())
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// operatorVerify asks the operator to verify the platform now, and answers 202
// with the request's token: the run is the operator's to start, and a client
// follows the document until operator.verification.request is that token and
// its result is no longer Running.
//
// A request while a run is in progress is accepted and waits for that run to
// end; a request while another one waits joins it, and answers with its token.
// Only a platform whose verification is switched off refuses one — 409,
// because the run it asks for would never come.
func (a *API) operatorVerify(w http.ResponseWriter, r *http.Request) {
	if !a.operatorReady(w) {
		return
	}
	// A request says nothing beyond itself; a body that is sent must be empty.
	var body struct{}
	if !decodeOptional(w, r, &body) {
		return
	}
	out, err := a.op.RequestVerification(r.Context())
	switch {
	case errors.Is(err, operator.ErrVerificationDisabled), k8s.IsConflict(err):
		http.Error(w, err.Error(), http.StatusConflict)
	case k8s.IsForbidden(err):
		http.Error(w, "portal-api may not write the operator's resource — update the zaentrum-operator, whose portal-api Role grants it: "+err.Error(),
			http.StatusServiceUnavailable)
	case err != nil:
		badRequest(w, err.Error())
	default:
		// Who asked, in the log: the request reaches the cluster as
		// portal-api's own service account, so this line is the only record
		// of the person behind it.
		if out.Joined {
			log.Printf("operator: %s asked for a verification; request %s was already waiting and answers it", requester(r), out.Request)
		} else {
			log.Printf("operator: %s asked for a verification: request %s", requester(r), out.Request)
		}
		writeJSON(w, http.StatusAccepted, out)
	}
}

// requester names the caller for the log: the username, else the subject.
func requester(r *http.Request) string {
	p, _ := auth.PrincipalFrom(r.Context())
	switch {
	case p == nil:
		return "an unknown caller"
	case p.Username != "":
		return p.Username
	case p.Subject != "":
		return p.Subject
	}
	return "an unknown caller"
}

func (a *API) instanceScale(w http.ResponseWriter, r *http.Request) {
	if !a.operatorReady(w) {
		return
	}
	var body struct {
		Replicas int `json:"replicas"`
	}
	if !decode(w, r, &body) {
		return
	}
	out, err := a.op.Scale(r.Context(), chi.URLParam(r, "name"), body.Replicas)
	a.instanceWrote(w, out, err)
}

func (a *API) instanceRestart(w http.ResponseWriter, r *http.Request) {
	if !a.operatorReady(w) {
		return
	}
	out, err := a.op.Restart(r.Context(), chi.URLParam(r, "name"))
	a.instanceWrote(w, out, err)
}

// instanceWrote answers a scale or a restart. A workload this namespace does
// not run is 404 — definitive, and a different thing from a refusal, which is
// what "protected" is and stays.
func (a *API) instanceWrote(w http.ResponseWriter, out operator.Write, err error) {
	switch {
	case errors.Is(err, operator.ErrNoWorkload):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		badRequest(w, err.Error())
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// operatorReady guards the write actions when instance management is unavailable.
func (a *API) operatorReady(w http.ResponseWriter) bool {
	if a.op == nil || !a.op.Available() {
		http.Error(w, "instance management is unavailable (not running in a cluster)", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// ─── reads ───────────────────────────────────────────────────────────────────

// launchpad is the launchpad as the caller may see it: spaces and tiles whose
// audience names none of the caller's roles are left out, server-side.
func (a *API) launchpad(w http.ResponseWriter, r *http.Request) {
	var roles []string
	if p, _ := auth.PrincipalFrom(r.Context()); p != nil {
		roles = p.Roles
	}
	lp, err := a.reg.Launchpad(r.Context(), roles)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lp)
}

// me says who the caller is and whether the console is theirs. isAdmin is the
// gate the admin routes apply — the admin role on a token of one of the
// portal's own clients — so a token that carries the role through another
// client reads false, and client says which one it came through.
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	out := map[string]any{"username": "", "roles": []string{}, "isAdmin": false, "adminRole": a.cfg.AdminRole, "client": ""}
	if p != nil {
		out["username"] = p.Username
		out["roles"] = nonNil(p.Roles)
		out["isAdmin"] = p.Admin
		out["client"] = p.Client
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── apps ────────────────────────────────────────────────────────────────────

func (a *API) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := a.reg.ListApps(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(apps))
}

func (a *API) upsertApp(w http.ResponseWriter, r *http.Request) {
	var app model.App
	if !decode(w, r, &app) {
		return
	}
	if strings.TrimSpace(app.Key) == "" || strings.TrimSpace(app.Title) == "" {
		badRequest(w, "app requires key and title")
		return
	}
	if app.Kind == "" {
		app.Kind = "tool"
	}
	if err := a.reg.UpsertApp(r.Context(), app); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (a *API) patchApp(w http.ResponseWriter, r *http.Request) {
	var app model.App
	if !decode(w, r, &app) {
		return
	}
	app.Key = chi.URLParam(r, "key")
	if strings.TrimSpace(app.Title) == "" {
		badRequest(w, "app requires title")
		return
	}
	if app.Kind == "" {
		app.Kind = "tool"
	}
	if err := a.reg.UpsertApp(r.Context(), app); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (a *API) deleteApp(w http.ResponseWriter, r *http.Request) {
	a.handleDelete(w, a.reg.DeleteApp(r.Context(), chi.URLParam(r, "key")))
}

// ─── UI extensions ─────────────────────────────────────────────────────────

// slotExtensions serves the enabled contributions for one slot (product-app
// read path — any signed-in user). A row written before the slot rules that
// breaks them is not served (servable).
func (a *API) slotExtensions(w http.ResponseWriter, r *http.Request) {
	exts, err := a.reg.ListExtensionsForSlot(r.Context(), chi.URLParam(r, "slot"))
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]model.Extension, 0, len(exts))
	for _, e := range exts {
		if e, ok := servable(e); ok {
			out = append(out, e)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// The extension routes take an admin, who manages every row, or an addon's
// service account, which manages its own addon's rows and nothing else: rows
// keyed <addon>.<name> that carry its addon, that no one else owns, of an
// addon installed here — and whose actions lead to its own API. An addon
// writing another's rows, or a row an admin made by hand, is refused.

// extensionCaller is the addon a caller of the extension routes writes for —
// "" for an admin — and false for a caller who is neither.
func extensionCaller(r *http.Request) (addon string, ok bool) {
	p, _ := auth.PrincipalFrom(r.Context())
	switch {
	case p == nil:
		return "", false
	case p.Admin:
		return "", true
	case p.Addon != "":
		return p.Addon, true
	}
	return "", false
}

// errNotTheAddons refuses an addon a row that is not its own.
type errNotTheAddons struct{ msg string }

func (e *errNotTheAddons) Error() string { return e.msg }

func notTheAddons(format string, args ...any) error {
	return &errNotTheAddons{msg: "forbidden: " + fmt.Sprintf(format, args...)}
}

// addonMayWrite checks that row is addon's to write.
func (a *API) addonMayWrite(ctx context.Context, addon string, row model.Extension) error {
	switch {
	case row.Addon != "" && row.Addon != addon:
		return notTheAddons("this token is addon %q's, and the row names addon %q — an addon writes its own rows", addon, row.Addon)
	case !strings.HasPrefix(row.Key, addon+"."):
		return notTheAddons("addon %q's rows are keyed %s.<name>; %q is not", addon, addon, row.Key)
	}
	switch existing, err := a.reg.GetExtension(ctx, row.Key); {
	case err == nil && existing.Addon != addon:
		return notTheAddons("row %q is not addon %q's — it belongs to %s", row.Key, addon, ownerOf(existing))
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}
	switch _, err := a.reg.GetApp(ctx, addon); {
	case errors.Is(err, store.ErrNotFound):
		return notTheAddons("addon %q is not installed on this instance — an admin adds it in settings → addons; its service account then keeps its own rows", addon)
	case err != nil:
		return err
	}
	return nil
}

func ownerOf(e *model.Extension) string {
	if e.Addon == "" {
		return "no addon: an admin made it"
	}
	return "addon " + strconv.Quote(e.Addon)
}

// extensionRefused answers a refused write: 403 for a row that is not the
// caller's, 500 for a registry that did not answer.
func extensionRefused(w http.ResponseWriter, err error) {
	var nt *errNotTheAddons
	if errors.As(err, &nt) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	serverError(w, err)
}

func (a *API) listExtensions(w http.ResponseWriter, r *http.Request) {
	addon, ok := extensionCaller(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	exts, err := a.reg.ListExtensions(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if addon != "" {
		own := exts[:0:0]
		for _, e := range exts {
			if e.Addon == addon {
				own = append(own, e)
			}
		}
		exts = own
	}
	writeJSON(w, http.StatusOK, nonNil(exts))
}

func (a *API) upsertExtension(w http.ResponseWriter, r *http.Request) {
	var e model.Extension
	if !decode(w, r, &e) {
		return
	}
	a.writeExtension(w, r, e, true)
}

func (a *API) patchExtension(w http.ResponseWriter, r *http.Request) {
	var e model.Extension
	if !decode(w, r, &e) {
		return
	}
	e.Key = chi.URLParam(r, "key")
	a.writeExtension(w, r, e, false)
}

// writeExtension checks a row and stores it. requireKey is true on create
// (POST) — PATCH takes the key from the path.
func (a *API) writeExtension(w http.ResponseWriter, r *http.Request, e model.Extension, requireKey bool) {
	addon, ok := extensionCaller(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if requireKey && strings.TrimSpace(e.Key) == "" {
		badRequest(w, "extension requires key")
		return
	}
	if strings.TrimSpace(e.Slot) == "" {
		badRequest(w, "extension requires slot")
		return
	}
	actions := ""
	if addon != "" {
		if err := a.addonMayWrite(r.Context(), addon, e); err != nil {
			extensionRefused(w, err)
			return
		}
		e.Addon, actions = addon, proxyPath(addon)
	}
	e, err := checkSlot(e, a.instanceOrigins(r), actions)
	if err != nil {
		badRequest(w, "extension "+err.Error())
		return
	}
	if err := a.reg.UpsertExtension(r.Context(), e); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (a *API) deleteExtension(w http.ResponseWriter, r *http.Request) {
	addon, ok := extensionCaller(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	key := chi.URLParam(r, "key")
	if addon != "" {
		existing, err := a.reg.GetExtension(r.Context(), key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "not found", http.StatusNotFound)
			return
		case err != nil:
			serverError(w, err)
			return
		case existing.Addon != addon || !strings.HasPrefix(key, addon+"."):
			extensionRefused(w, notTheAddons("row %q is not addon %q's — it belongs to %s", key, addon, ownerOf(existing)))
			return
		}
	}
	a.handleDelete(w, a.reg.DeleteExtension(r.Context(), key))
}

// ─── spaces ──────────────────────────────────────────────────────────────────

func (a *API) listSpaces(w http.ResponseWriter, r *http.Request) {
	spaces, err := a.reg.ListSpaces(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(spaces))
}

func (a *API) upsertSpace(w http.ResponseWriter, r *http.Request) {
	var sp model.Space
	if !decode(w, r, &sp) {
		return
	}
	if strings.TrimSpace(sp.Key) == "" || strings.TrimSpace(sp.Title) == "" {
		badRequest(w, "space requires key and title")
		return
	}
	a.writeSpace(w, r, sp)
}

func (a *API) patchSpace(w http.ResponseWriter, r *http.Request) {
	var sp model.Space
	if !decode(w, r, &sp) {
		return
	}
	sp.Key = chi.URLParam(r, "key")
	if strings.TrimSpace(sp.Title) == "" {
		badRequest(w, "space requires title")
		return
	}
	a.writeSpace(w, r, sp)
}

// writeSpace stores a space and answers with it as stored — an audience the
// request left out is the one kept.
func (a *API) writeSpace(w http.ResponseWriter, r *http.Request, sp model.Space) {
	var err error
	if sp.Audience, err = cleanAudience(sp.Audience); err != nil {
		badRequest(w, "space "+err.Error())
		return
	}
	if err := a.reg.UpsertSpace(r.Context(), sp); err != nil {
		serverError(w, err)
		return
	}
	stored, err := a.reg.GetSpace(r.Context(), sp.Key)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

func (a *API) deleteSpace(w http.ResponseWriter, r *http.Request) {
	a.handleDelete(w, a.reg.DeleteSpace(r.Context(), chi.URLParam(r, "key")))
}

// ─── tiles ───────────────────────────────────────────────────────────────────

func (a *API) listTiles(w http.ResponseWriter, r *http.Request) {
	tiles, err := a.reg.ListTiles(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(tiles))
}

func (a *API) upsertTile(w http.ResponseWriter, r *http.Request) {
	var t model.Tile
	if !decode(w, r, &t) {
		return
	}
	if !a.validTile(w, t, false) {
		return
	}
	a.writeTile(w, r, t)
}

func (a *API) patchTile(w http.ResponseWriter, r *http.Request) {
	var t model.Tile
	if !decode(w, r, &t) {
		return
	}
	t.Key = chi.URLParam(r, "key")
	if !a.validTile(w, t, true) {
		return
	}
	a.writeTile(w, r, t)
}

// writeTile stores a tile and answers with it as stored — an audience the
// request left out is the one kept.
func (a *API) writeTile(w http.ResponseWriter, r *http.Request, t model.Tile) {
	var err error
	if t.Audience, err = cleanAudience(t.Audience); err != nil {
		badRequest(w, "tile "+err.Error())
		return
	}
	if err := a.reg.UpsertTile(r.Context(), t); err != nil {
		a.tileWriteError(w, err)
		return
	}
	stored, err := a.reg.GetTile(r.Context(), t.Key)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// Audience limits: generous for a realm's roles, small enough to render.
const (
	maxAudience     = 32
	maxAudienceRole = 255
)

// cleanAudience reads an audience as an admin wrote it: realm role names,
// trimmed, each once. nil — the request left it out — stays nil, and the
// store keeps what it has; an empty list is everyone signed in.
func cleanAudience(in []string) ([]string, error) {
	if in == nil {
		return nil, nil
	}
	out := []string{}
	for _, role := range in {
		role = strings.TrimSpace(role)
		switch {
		case role == "":
			continue
		case len(role) > maxAudienceRole || hasControl(role) || strings.ContainsAny(role, " ,"):
			return nil, fmt.Errorf("audience: %q is no realm role name", role)
		case !slices.Contains(out, role):
			out = append(out, role)
		}
	}
	if len(out) > maxAudience {
		return nil, fmt.Errorf("audience: at most %d roles", maxAudience)
	}
	return out, nil
}

func (a *API) deleteTile(w http.ResponseWriter, r *http.Request) {
	a.handleDelete(w, a.reg.DeleteTile(r.Context(), chi.URLParam(r, "key")))
}

func (a *API) validTile(w http.ResponseWriter, t model.Tile, patch bool) bool {
	if !patch && strings.TrimSpace(t.Key) == "" {
		badRequest(w, "tile requires key")
		return false
	}
	if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.AppKey) == "" || strings.TrimSpace(t.SpaceKey) == "" {
		badRequest(w, "tile requires title, appKey and spaceKey")
		return false
	}
	if t.Open != "" && t.Open != "inline" && t.Open != "newtab" {
		badRequest(w, `tile open must be "inline", "newtab", or empty`)
		return false
	}
	return true
}

func (a *API) tileWriteError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "does not exist") {
		badRequest(w, err.Error())
		return
	}
	serverError(w, err)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (a *API) handleDelete(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		badRequest(w, "invalid json: "+err.Error())
		return false
	}
	return true
}

// decodeOptional reads a body a caller may leave out entirely — an empty body
// leaves dst as it was. Anything actually sent must still parse, unknown
// fields and all: "optional" is about the body, not about its contents.
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		badRequest(w, "invalid json: "+err.Error())
		return false
	}
	return true
}

// ─── debug: container logs ─────────────────────────────────────────────────────

// debugPods lists the namespace's pods + their container names for the log
// viewer's selector. Empty list when not in-cluster (dev / appliance).
func (a *API) debugPods(w http.ResponseWriter, r *http.Request) {
	if !a.op.Available() {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	pods, err := a.op.LogPods(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pods)
}

// debugLogs returns a pod container's recent logs (secrets redacted) as text.
// Query: pod (required), container, tail (lines), since (seconds).
func (a *API) debugLogs(w http.ResponseWriter, r *http.Request) {
	if !a.op.Available() {
		http.Error(w, "log viewer is unavailable (not running in a cluster)", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	pod := q.Get("pod")
	if strings.TrimSpace(pod) == "" {
		badRequest(w, "pod is required")
		return
	}
	tail, _ := strconv.Atoi(q.Get("tail"))
	since, _ := strconv.Atoi(q.Get("since"))
	logs, err := a.op.Logs(r.Context(), pod, q.Get("container"), tail, since)
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

// ─── debug: kafka event tap ────────────────────────────────────────────────────

// kafkaTopology returns the live bus view: prefixed topics, their partitions, the
// consumer groups bound to each, and the tap's own observed activity. Degrades to
// {available:false} when no bus is wired (dev / appliance).
func (a *API) kafkaTopology(w http.ResponseWriter, r *http.Request) {
	if a.tap == nil || !a.tap.Available() {
		writeJSON(w, http.StatusOK, eventtap.Topology{Available: false, Note: "Kafka introspection is unavailable (KAFKA_BROKERS unset)"})
		return
	}
	writeJSON(w, http.StatusOK, a.tap.Topology(r.Context()))
}

// kafkaEvents returns recent observed events (newest first, secrets redacted),
// optionally filtered to ?topic= and capped by ?limit=.
func (a *API) kafkaEvents(w http.ResponseWriter, r *http.Request) {
	if a.tap == nil || !a.tap.Available() {
		writeJSON(w, http.StatusOK, []eventtap.Event{})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, http.StatusOK, nonNil(a.tap.Events(q.Get("topic"), limit)))
}

// ─── debug: curated read-only db browser ───────────────────────────────────────

// dbTables lists the curated (whitelisted) tables/views with live row counts.
func (a *API) dbTables(w http.ResponseWriter, r *http.Request) {
	if a.br == nil || !a.br.Available() {
		writeJSON(w, http.StatusOK, []dbbrowse.Table{})
		return
	}
	writeJSON(w, http.StatusOK, a.br.Tables(r.Context()))
}

// dbRows returns a page of a curated table (read-only, secrets masked). Query:
// table (required, must be whitelisted), limit, offset.
func (a *API) dbRows(w http.ResponseWriter, r *http.Request) {
	if a.br == nil || !a.br.Available() {
		http.Error(w, "db browser is unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("table")) == "" {
		badRequest(w, "table is required")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	page, err := a.br.Rows(r.Context(), q.Get("table"), limit, offset)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown table") {
			badRequest(w, err.Error())
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// ─── debug: support bundle ─────────────────────────────────────────────────────

// supportBundle assembles a downloadable diagnostic bundle from the sections the
// caller opted into (?logs=&instances=&kafka=&registry=&config=, each default on,
// set to 0 to omit). Everything is secret-scrubbed twice: per-section (logs use
// the same ScrubSecrets as the live viewer) and once more over the final JSON as
// a belt-and-braces net. Never includes DB credentials or bearer tokens.
func (a *API) supportBundle(w http.ResponseWriter, r *http.Request) {
	// Self-cap the whole assembly: the per-container log walk is sequential and
	// each apiserver call can take up to the k8s client's timeout, so bound the
	// total so a slow/large namespace can't hold the request (and its growing
	// in-memory bundle) open indefinitely.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	q := r.URL.Query()
	// A section is included unless explicitly disabled (?x=0 / ?x=false).
	on := func(k string) bool {
		v := strings.ToLower(strings.TrimSpace(q.Get(k)))
		return v != "0" && v != "false" && v != "off"
	}

	bundle := map[string]any{
		"kind":        "zaentrum-support-bundle",
		"version":     1,
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
	}
	sections := map[string]any{}

	if on("config") {
		sections["config"] = a.configSummary()
	}
	if on("registry") {
		apps, _ := a.reg.ListApps(ctx)
		spaces, _ := a.reg.ListSpaces(ctx)
		tiles, _ := a.reg.ListTiles(ctx)
		sections["registry"] = map[string]any{
			"apps": nonNil(apps), "spaces": nonNil(spaces), "tiles": nonNil(tiles),
		}
	}
	if on("kafka") && a.tap != nil && a.tap.Available() {
		sections["kafka"] = a.tap.Topology(ctx)
	}
	if a.op != nil && a.op.Available() {
		bundle["namespace"] = a.op.Namespace()
		if on("instances") {
			info, _ := a.op.OperatorInfo(ctx)
			inst, _ := a.op.Instances(ctx)
			sections["operator"] = info
			sections["instances"] = nonNil(inst)
		}
		if on("logs") || on("pods") {
			pods, _ := a.op.LogPods(ctx)
			sections["pods"] = nonNil(pods)
			if on("logs") {
				// Each container is byte-capped by operator.Logs (maxLogBytes); this
				// aggregate cap bounds the whole bundle so no combination of pods can
				// exceed portal-api's memory budget.
				const maxBundleLogBytes = 24 << 20 // 24 MiB
				logs := map[string]string{}
				total, truncated := 0, false
			collect:
				for _, p := range pods {
					for _, c := range p.Containers {
						if total >= maxBundleLogBytes {
							truncated = true
							break collect
						}
						txt, err := a.op.Logs(ctx, p.Pod, c, 200, 0)
						if err != nil {
							continue
						}
						logs[p.Pod+"/"+c] = txt
						total += len(txt)
					}
				}
				if truncated {
					logs["_note"] = "truncated: support-bundle log size cap reached"
				}
				sections["logs"] = logs
			}
		}
	}
	bundle["sections"] = sections

	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		serverError(w, err)
		return
	}
	// Final safety net: scrub the whole serialized document once more.
	safe := redact.Secrets(string(raw))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="zaentrum-support-bundle.json"`)
	_, _ = w.Write([]byte(safe))
}

// configSummary returns non-secret runtime configuration for the bundle — never
// the DB user/password or any credential.
func (a *API) configSummary() map[string]any {
	return map[string]any{
		"oidcIssuer":       a.cfg.OIDCIssuer,
		"audience":         a.cfg.Audience,
		"audienceRequired": a.cfg.AudienceRequired,
		"adminRole":        a.cfg.AdminRole,
		"adminClients":     a.cfg.AdminClients,
		"instanceSelector": a.cfg.InstanceSelector,
		"protectedNames":   a.cfg.ProtectedNames,
		"operatorGroup":    a.cfg.OperatorGroup,
		"operatorVersion":  a.cfg.OperatorVersion,
		"operatorPlural":   a.cfg.OperatorPlural,
		"addonPlural":      a.cfg.AddonPlural,
		"kafkaBrokers":     a.cfg.KafkaBrokers,
		"kafkaTopicPrefix": a.cfg.KafkaTopicPrefix,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

func serverError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// nonNil renders an empty slice as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
