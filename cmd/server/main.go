// Package main is the CRM HTTP server entrypoint. The wire-up here is
// the *only* place where concrete adapters import database/sql, NATS,
// Prometheus client_golang, etc. — the domain (internal/webhook) and the
// reconciler (internal/worker) stay SDK-free per ADR 0075.
//
// Feature flag: WEBHOOK_SECURITY_V2_ENABLED. Default *off* — the server
// answers 200 OK on /webhooks/... via stubWebhookHandler and never opens
// a Postgres pool. When set to true the stack is constructed (Meta
// adapters + Postgres stores + Prometheus metrics + slog + reconciler
// goroutine) and the JetStream Duplicates ≥ 1h check fails-fast on
// misconfiguration (ADR 0075 rev 3 / F-14).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultAddr = ":8080"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Getenv))
}

// execute is the testable entrypoint. It loads config from getenv,
// builds the webhook stack, starts the reconciler goroutine when
// applicable, and serves until ctx is done.
func execute(ctx context.Context, getenv func(string) string) int {
	cfg, err := loadConfig(getenv)
	if err != nil {
		log.Printf("crm: config: %v", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	stack, err := buildStack(ctx, cfg, logger, defaultPoolOpener)
	if err != nil {
		log.Printf("crm: stack: %v", err)
		return 1
	}
	defer stack.Close()

	if stack.reconciler != nil {
		go func() {
			if err := stack.reconciler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("reconciler exited", slog.String("error", err.Error()))
			}
		}()
	}

	if err := serve(ctx, cfg.HTTPAddr, buildMux(stack)); err != nil {
		log.Printf("crm: %v", err)
		return 1
	}
	return 0
}

// run preserves the original 2-arg signature used by main_test.go. It
// serves the basic /health-only mux returned by newMux().
func run(ctx context.Context, addr string) error {
	return serve(ctx, addr, newMux())
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("crm: listening on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newMux returns the basic mux with /health only. Kept for its existing
// tests; the production binary uses buildMux on top of a fully wired
// stack instead.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	return mux
}

// healthHandler is the basic 200-OK liveness probe used by newMux.
// buildMux replaces it with healthHandlerFor in the production path so
// readiness reflects the reconciler's most recent tick.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
