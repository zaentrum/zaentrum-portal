// Package api exposes the portal registry over REST. The launchpad + identity
// reads are available to any signed-in user; apps/spaces/tiles writes are gated
// on the realm admin role by the router (see auth.Middleware.RequireAdmin).
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

type API struct {
	st  *store.Store
	cfg config.Config
	op  *operator.Service
}

func New(st *store.Store, cfg config.Config, op *operator.Service) *API {
	return &API{st: st, cfg: cfg, op: op}
}

// Register mounts the registry routes under /api/portal. Authn is applied by the
// caller's group; admin writes are additionally gated here via mw.RequireAdmin.
func (a *API) Register(r chi.Router, mw *auth.Middleware) {
	r.Route("/api/portal", func(r chi.Router) {
		// Reads for any signed-in user.
		r.Get("/launchpad", a.launchpad)
		r.Get("/me", a.me)

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
