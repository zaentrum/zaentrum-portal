// Package api exposes the portal registry over REST. The launchpad + identity
// reads are available to any signed-in user; apps/spaces/tiles writes are gated
// on the realm admin role by the router (see auth.Middleware.RequireAdmin).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/dbbrowse"
	"github.com/zaentrum/zaentrum-portal/server/internal/eventtap"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/redact"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

type API struct {
	st  *store.Store
	cfg config.Config
	op  *operator.Service
	tap *eventtap.Tap
	br  *dbbrowse.Browser
}

func New(st *store.Store, cfg config.Config, op *operator.Service, tap *eventtap.Tap, br *dbbrowse.Browser) *API {
	return &API{st: st, cfg: cfg, op: op, tap: tap, br: br}
}

// Register mounts the registry routes under /api/portal. Authn is applied by the
// caller's group; admin writes are additionally gated here via mw.RequireAdmin.
func (a *API) Register(r chi.Router, mw *auth.Middleware) {
	r.Route("/api/portal", func(r chi.Router) {
		// Reads for any signed-in user.
		r.Get("/launchpad", a.launchpad)
		r.Get("/me", a.me)
		// Product apps read the enabled extension contributions for a slot
		// (chino forwards the user's bearer here). Any signed-in user.
		r.Get("/slots/{slot}", a.slotExtensions)

		// Registry administration (settings console).
		r.Group(func(ar chi.Router) {
			ar.Use(mw.RequireAdmin)

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
	if err := a.op.SetOperator(r.Context(), body.Version, body.Channel, body.UpdateMode); err != nil {
		badRequest(w, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) operatorApplyUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.operatorReady(w) {
		return
	}
	if err := a.op.ApplyUpdate(r.Context()); err != nil {
		badRequest(w, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if err := a.op.Scale(r.Context(), chi.URLParam(r, "name"), body.Replicas); err != nil {
		badRequest(w, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) instanceRestart(w http.ResponseWriter, r *http.Request) {
	if !a.operatorReady(w) {
		return
	}
	if err := a.op.Restart(r.Context(), chi.URLParam(r, "name")); err != nil {
		badRequest(w, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func (a *API) launchpad(w http.ResponseWriter, r *http.Request) {
	lp, err := a.st.Launchpad(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lp)
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	out := map[string]any{"username": "", "roles": []string{}, "isAdmin": false}
	if p != nil {
		out["username"] = p.Username
		out["roles"] = p.Roles
		out["isAdmin"] = p.HasRole(a.cfg.AdminRole)
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── apps ────────────────────────────────────────────────────────────────────

func (a *API) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := a.st.ListApps(r.Context())
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
	if err := a.st.UpsertApp(r.Context(), app); err != nil {
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
	if err := a.st.UpsertApp(r.Context(), app); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (a *API) deleteApp(w http.ResponseWriter, r *http.Request) {
	a.handleDelete(w, a.st.DeleteApp(r.Context(), chi.URLParam(r, "key")))
}

// ─── UI extensions ─────────────────────────────────────────────────────────

// slotExtensions serves the enabled contributions for one slot (product-app
// read path — any signed-in user).
func (a *API) slotExtensions(w http.ResponseWriter, r *http.Request) {
	exts, err := a.st.ListExtensionsForSlot(r.Context(), chi.URLParam(r, "slot"))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(exts))
}

func (a *API) listExtensions(w http.ResponseWriter, r *http.Request) {
	exts, err := a.st.ListExtensions(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(exts))
}

func (a *API) upsertExtension(w http.ResponseWriter, r *http.Request) {
	var e model.Extension
	if !decode(w, r, &e) {
		return
	}
	if !validExtension(w, e, true) {
		return
	}
	if err := a.st.UpsertExtension(r.Context(), normExtension(e)); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, normExtension(e))
}

func (a *API) patchExtension(w http.ResponseWriter, r *http.Request) {
	var e model.Extension
	if !decode(w, r, &e) {
		return
	}
	e.Key = chi.URLParam(r, "key")
	if !validExtension(w, e, false) {
		return
	}
	if err := a.st.UpsertExtension(r.Context(), normExtension(e)); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, normExtension(e))
}

func (a *API) deleteExtension(w http.ResponseWriter, r *http.Request) {
	a.handleDelete(w, a.st.DeleteExtension(r.Context(), chi.URLParam(r, "key")))
}

// validExtension enforces the required fields and the kind enum. requireKey is
// true on create (POST) — PATCH takes the key from the path.
func validExtension(w http.ResponseWriter, e model.Extension, requireKey bool) bool {
	if requireKey && strings.TrimSpace(e.Key) == "" {
		badRequest(w, "extension requires key")
		return false
	}
	if strings.TrimSpace(e.Slot) == "" {
		badRequest(w, "extension requires slot")
		return false
	}
	if e.Kind != "" && e.Kind != "link" && e.Kind != "action" {
		badRequest(w, "extension kind must be 'link' or 'action'")
		return false
	}
	return true
}

// normExtension fills defaults (kind=link, method=POST).
func normExtension(e model.Extension) model.Extension {
	if e.Kind == "" {
		e.Kind = "link"
	}
	if e.Method == "" {
		e.Method = "POST"
	}
	return e
}

// ─── spaces ──────────────────────────────────────────────────────────────────

func (a *API) listSpaces(w http.ResponseWriter, r *http.Request) {
	spaces, err := a.st.ListSpaces(r.Context())
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
	if err := a.st.UpsertSpace(r.Context(), sp); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
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
	if err := a.st.UpsertSpace(r.Context(), sp); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (a *API) deleteSpace(w http.ResponseWriter, r *http.Request) {
	a.handleDelete(w, a.st.DeleteSpace(r.Context(), chi.URLParam(r, "key")))
}

// ─── tiles ───────────────────────────────────────────────────────────────────

func (a *API) listTiles(w http.ResponseWriter, r *http.Request) {
	tiles, err := a.st.ListTiles(r.Context())
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
	if err := a.st.UpsertTile(r.Context(), t); err != nil {
		a.tileWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
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
	if err := a.st.UpsertTile(r.Context(), t); err != nil {
		a.tileWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (a *API) deleteTile(w http.ResponseWriter, r *http.Request) {
	a.handleDelete(w, a.st.DeleteTile(r.Context(), chi.URLParam(r, "key")))
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
		apps, _ := a.st.ListApps(ctx)
		spaces, _ := a.st.ListSpaces(ctx)
		tiles, _ := a.st.ListTiles(ctx)
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
		"instanceSelector": a.cfg.InstanceSelector,
		"protectedNames":   a.cfg.ProtectedNames,
		"operatorGroup":    a.cfg.OperatorGroup,
		"operatorVersion":  a.cfg.OperatorVersion,
		"operatorPlural":   a.cfg.OperatorPlural,
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
