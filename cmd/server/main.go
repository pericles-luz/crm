// Package main is the CRM HTTP server entrypoint (SIN-62208 Fase 0 PR1).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	uploadweb "github.com/pericles-luz/crm/internal/adapter/web/upload"
)

const defaultAddr = ":8080"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Getenv))
}

func execute(ctx context.Context, getenv func(string) string) int {
	addr := defaultAddr
	if v := getenv("HTTP_ADDR"); v != "" {
		addr = v
	}
	if err := run(ctx, addr); err != nil {
		log.Printf("crm: %v", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(),
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

// uploadAttachmentFormEnabled gates the message-attachment form (Fase 2
// in the SIN-62226 plan). Defaults to false — the route 404s — until the
// message handler ships and the operator opts in by setting
// SIN_UPLOAD_ATTACHMENT_FORM=1.
func uploadAttachmentFormEnabled() bool {
	v := strings.TrimSpace(os.Getenv("SIN_UPLOAD_ATTACHMENT_FORM"))
	return v == "1" || strings.EqualFold(v, "true")
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)

	mux.Handle("/uploads/logo", uploadFormHandler(uploadweb.KindLogo, true))
	mux.Handle("/uploads/attachment", uploadFormHandler(uploadweb.KindAttachment, uploadAttachmentFormEnabled()))
	mux.Handle(
		"/static/upload/",
		http.StripPrefix("/static/upload/", uploadweb.StaticHandler()),
	)
	return mux
}

// uploadFormHandler renders an upload form. Only GET is supported here —
// the corresponding POST/PUT receiver is a separate ticket. When enabled
// is false (e.g. attachment in Fase 1) the route 404s so the form is not
// even discoverable.
func uploadFormHandler(kind uploadweb.Kind, enabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// No-cache: forms may carry CSRF tokens or feature-flag-dependent
		// HTML, so don't let proxies pin them.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if err := uploadweb.Render(w, kind, uploadweb.FormConfig{}); err != nil {
			log.Printf("crm: render upload form %s: %v", kind, err)
		}
	})
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
