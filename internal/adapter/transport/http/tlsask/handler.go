// Package tlsask is the HTTP transport adapter for the Caddy
// on_demand_tls.ask handler (SIN-62243 F45). It owns nothing but the wire
// protocol — the policy lives in the use-case at
// internal/customdomain/tls_ask.
//
// Wire contract (matches Caddy's on_demand_tls expectations):
//
//	GET /internal/tls/ask?domain=<host>
//	200 OK              -> Caddy may issue a certificate for <host>.
//	403 Forbidden       -> deny (unknown / unverified / paused / invalid).
//	429 Too Many Requests -> per-host budget exceeded (Retry-After: 60).
//	503 Service Unavailable -> customdomain.ask_enabled = false.
//	500 Internal Server Error -> port failure; Caddy will retry on its own.
//
// The body is a small JSON envelope with the structured reason for human
// debugging. Caddy itself only inspects the status code, so the body is
// purely for ops.
package tlsask

import (
	"encoding/json"
	"net/http"

	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

// Path is the route the handler is registered at. Exposed so cmd/server
// and the integration test agree on the spelling.
const Path = "/internal/tls/ask"

// Handler wraps the use-case in an http.Handler. It is safe for concurrent
// use as long as the embedded use-case is.
type Handler struct {
	uc *tls_ask.UseCase
}

// New returns a Handler. The use-case must be fully wired (repo, rate,
// flag, log) before being passed in.
func New(uc *tls_ask.UseCase) *Handler {
	return &Handler{uc: uc}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, response{
			Status: "error",
			Reason: "method_not_allowed",
		})
		return
	}
	host := r.URL.Query().Get("domain")
	if host == "" {
		writeJSON(w, http.StatusForbidden, response{
			Status: "deny",
			Reason: tls_ask.ReasonInvalidHost.String(),
		})
		return
	}

	res := h.uc.Ask(r.Context(), host)
	switch res.Decision {
	case tls_ask.DecisionAllow:
		writeJSON(w, http.StatusOK, response{Status: "allow", Host: res.Host})
	case tls_ask.DecisionRateLimited:
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, response{
			Status: "rate_limited",
			Host:   res.Host,
		})
	case tls_ask.DecisionDisabled:
		writeJSON(w, http.StatusServiceUnavailable, response{
			Status: "disabled",
			Reason: res.Reason.String(),
			Host:   res.Host,
			// Human-readable hint, not contractually load-bearing.
			Message: "customdomain.ask_enabled is OFF; on-demand TLS issuance is paused",
		})
	case tls_ask.DecisionDeny:
		writeJSON(w, http.StatusForbidden, response{
			Status: "deny",
			Reason: res.Reason.String(),
			Host:   res.Host,
		})
	default: // DecisionError, DecisionUnknown
		writeJSON(w, http.StatusInternalServerError, response{
			Status: "error",
			Reason: res.Reason.String(),
			Host:   res.Host,
		})
	}
}

type response struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Host    string `json:"host,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, body response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
