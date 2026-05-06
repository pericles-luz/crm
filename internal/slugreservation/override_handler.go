package slugreservation

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
)

// MaxOverrideBodyBytes caps the override request body. The payload is
// tiny ({reason: "..."}); 4 KiB is generous and keeps the boundary
// honest.
const MaxOverrideBodyBytes = 4 << 10

// OverrideRequireMFA, when true, makes the master override handler
// reject any authorization where mfaPresent is false. The flag exists
// so F46 can land before F15 (RequireMasterMFA middleware) — set it to
// false in environments still on the interim flow, true everywhere
// once F15 ships. ADR 0079 §4 documents the gate.
//
// We do NOT default this to false in the constructor; cmd/server reads
// it from env so the deployment posture is explicit.

// OverrideHandler serves POST /api/master/slug-reservations/:slug/release.
type OverrideHandler struct {
	svc        *Service
	auth       MasterAuthorizer
	requireMFA bool
}

// NewOverrideHandler builds the handler. Pass requireMFA=true once
// SIN-62223 RequireMasterMFA is the source of truth for the mfaPresent
// bit; until then the handler can run with requireMFA=false and still
// enforce master via auth.AuthorizeMaster.
func NewOverrideHandler(svc *Service, auth MasterAuthorizer, requireMFA bool) *OverrideHandler {
	return &OverrideHandler{svc: svc, auth: auth, requireMFA: requireMFA}
}

// Register attaches the route to a Go 1.22 stdlib mux.
func (h *OverrideHandler) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/master/slug-reservations/{slug}/release", h)
}

type overrideRequest struct {
	Reason string `json:"reason"`
}

type overrideResponse struct {
	Slug      string `json:"slug"`
	ExpiresAt string `json:"expiresAt"`
	Status    string `json:"status"`
}

// ServeHTTP implements http.Handler.
func (h *OverrideHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	masterID, mfa, err := h.auth.AuthorizeMaster(r.Context())
	if err != nil || masterID == uuid.Nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.requireMFA && !mfa {
		http.Error(w, "mfa required", http.StatusForbidden)
		return
	}
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxOverrideBodyBytes))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req overrideRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
	}

	res, err := h.svc.OverrideRelease(r.Context(), slug, masterID, req.Reason)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, overrideResponse{
			Slug:      res.Slug,
			ExpiresAt: res.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			Status:    "released",
		})
	case errors.Is(err, ErrInvalidSlug):
		writeJSON(w, http.StatusBadRequest, invalidBody{Error: "invalid slug"})
	case errors.Is(err, ErrReasonRequired):
		writeJSON(w, http.StatusBadRequest, invalidBody{Error: "reason required"})
	case errors.Is(err, ErrZeroMaster):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, ErrNotReserved):
		http.Error(w, "no active reservation", http.StatusNotFound)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
