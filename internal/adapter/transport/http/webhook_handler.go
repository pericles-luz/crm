// Package http hosts the HTTP boundary for the webhook intake. The
// handler is intentionally small: read the body once (ADR §2 D2 body
// bit-exactness), hand off to webhook.Service, always answer 200 OK.
//
// Routing uses Go 1.22 stdlib path patterns:
//
//	mux.Handle("POST /webhooks/{channel}/{webhook_token}", h)
package http

import (
	"io"
	"net/http"

	"github.com/pericles-luz/crm/internal/webhook"
)

// MaxBodyBytes caps inbound body size at 1 MiB. Meta envelopes are well
// under 50 KiB in practice; the cap protects the process against a
// misconfigured upstream.
const MaxBodyBytes = 1 << 20

// Handler exposes ServeHTTP and a Register helper for both stdlib mux
// and chi.
type Handler struct {
	svc *webhook.Service
}

// NewHandler returns a Handler that delegates verification, dedup, and
// publish to svc.
func NewHandler(svc *webhook.Service) *Handler { return &Handler{svc: svc} }

// Register attaches the handler to a Go 1.22 stdlib mux at the canonical
// path. The path values are read by ServeHTTP via PathValue.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /webhooks/{channel}/{webhook_token}", h)
}

// ServeHTTP reads the body once and dispatches to the service. The
// response is always 200 OK with an empty body — anti-enumeration per
// ADR §2 D5. Outcome details flow through metrics/logs only.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	channel := r.PathValue("channel")
	token := r.PathValue("webhook_token")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		// ReadAll failure (oversize / IO) — silent 200 like every other
		// rejection path. The byte count metric on the proxy alerts us.
		writeAck(w)
		return
	}

	req := webhook.Request{
		Channel:   channel,
		Token:     token,
		Body:      body,
		Headers:   cloneHeaders(r.Header),
		RequestID: r.Header.Get("X-Request-Id"),
	}
	_ = h.svc.Handle(r.Context(), req)
	writeAck(w)
}

func writeAck(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// cloneHeaders flattens http.Header (a map[string][]string) into the
// shape the domain consumes. Keeping a separate type alias here keeps
// internal/webhook free of net/http imports.
func cloneHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}
