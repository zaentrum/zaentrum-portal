// Command server is the portal-api: the launchpad registry (apps / spaces /
// tiles) behind the zaentrum portal. It serves the assembled launchpad to any
// signed-in user and CRUD to realm admins (the settings console). Folded into
// the zaentrum-portal repo — the shell configuring itself — it builds as its
// own image and runs as its own Deployment.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/zaentrum/zaentrum-portal/server/db"
	"github.com/zaentrum/zaentrum-portal/server/internal/api"
	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg := config.Load()

	// Server-lifetime context; cancelled on shutdown so the OIDC retry
	// goroutine stops with the server.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	st, err := store.New(bgCtx, cfg.DatabaseURL, cfg.DatabaseUser, cfg.DatabasePassword)
	if err != nil {
		return err
	}
	defer st.Close()

	// Apply the embedded, idempotent schema + seed on boot (no init job).
	if err := st.Migrate(bgCtx, db.Migrations); err != nil {
		return err
	}

	jwt, err := auth.NewJWTVerifier(bgCtx, cfg.OIDCIssuer, cfg.Audience, cfg.AudienceRequired, cfg.AuthDisabled)
	if err != nil {
		return err
	}
	authMW := auth.NewMiddleware(jwt, cfg.AdminRole)

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Health (public — also whitelisted in the auth middleware).
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeText(w, "ok\n") })
	r.Get("/actuator/health/liveness", func(w http.ResponseWriter, _ *http.Request) { writeText(w, `{"status":"UP"}`) })
	r.Get("/actuator/health/readiness", func(w http.ResponseWriter, req *http.Request) {
		if err := st.Ping(req.Context()); err != nil {
			http.Error(w, `{"status":"DOWN"}`, http.StatusServiceUnavailable)
			return
		}
		writeText(w, `{"status":"UP"}`)
	})

	// Authenticated surface.
	r.Group(func(pr chi.Router) {
		pr.Use(authMW.Authn)
		api.New(st, cfg).Register(pr, authMW)
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("portal-api listening on :%s (registry /api/portal)", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s))
}
