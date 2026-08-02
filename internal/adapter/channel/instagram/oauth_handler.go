package instagram

// OAuthCallbackHandler is the HTTP entry point Instagram redirects the
// operator's browser back to after Business Login for Instagram
// (mounted at OAuthCallbackPath on the public mux — see
// cmd/server/instagram_oauth_wire.go). It is stateless: the tenant is
// resolved entirely from the signed `state` query param (see
// oauth_state.go), not from session/tenant middleware, so it works even
// if this process instance didn't issue the original redirect.

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// OAuthTokenSaver persists the long-lived token this handler obtains.
type OAuthTokenSaver interface {
	Save(ctx context.Context, tenantID uuid.UUID, accessToken, tokenType string, expiresAt time.Time) error
}

// OAuthCallbackHandler exchanges the authorization code for a long-lived
// token and stores it. Implements http.Handler.
type OAuthCallbackHandler struct {
	cfg         OAuthConfig
	stateSecret []byte
	store       OAuthTokenSaver
}

// NewOAuthCallbackHandler builds an OAuthCallbackHandler. cfg, stateSecret,
// and store are all required.
func NewOAuthCallbackHandler(cfg OAuthConfig, stateSecret []byte, store OAuthTokenSaver) *OAuthCallbackHandler {
	return &OAuthCallbackHandler{cfg: cfg, stateSecret: stateSecret, store: store}
}

// settingsChannelsPath is the operator-facing landing page this handler
// redirects back to, on the SAME host the request arrived on (the
// browser still holds that host's session cookie, so this lands the
// operator back in the authenticated app instead of a bare static page).
const settingsChannelsPath = "/settings/channels"

func (h *OAuthCallbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	landingBase := requestScheme(r) + "://" + r.Host + settingsChannelsPath

	if errParam := q.Get("error"); errParam != "" {
		log.Printf("crm: instagram oauth callback — user denied or Instagram error: %s (%s)", errParam, q.Get("error_description"))
		http.Redirect(w, r, landingBase+"?instagram=error", http.StatusFound)
		return
	}

	state := q.Get("state")
	code := q.Get("code")
	if state == "" || code == "" {
		log.Printf("crm: instagram oauth callback — missing state or code")
		http.Redirect(w, r, landingBase+"?instagram=error", http.StatusFound)
		return
	}

	tenantID, err := VerifyOAuthState(h.stateSecret, state)
	if err != nil {
		log.Printf("crm: instagram oauth callback — invalid state: %v", err)
		http.Redirect(w, r, landingBase+"?instagram=error", http.StatusFound)
		return
	}

	redirectURI := requestScheme(r) + "://" + r.Host + OAuthCallbackPath

	shortLived, err := h.cfg.ExchangeCode(r.Context(), redirectURI, code)
	if err != nil {
		log.Printf("crm: instagram oauth callback — code exchange failed for tenant %s: %v", tenantID, err)
		http.Redirect(w, r, landingBase+"?instagram=error", http.StatusFound)
		return
	}

	accessToken, ttl, err := h.cfg.ExchangeLongLivedToken(r.Context(), shortLived)
	if err != nil {
		log.Printf("crm: instagram oauth callback — long-lived upgrade failed for tenant %s: %v", tenantID, err)
		http.Redirect(w, r, landingBase+"?instagram=error", http.StatusFound)
		return
	}

	if err := h.store.Save(r.Context(), tenantID, accessToken, "bearer", time.Now().Add(ttl)); err != nil {
		log.Printf("crm: instagram oauth callback — token save failed for tenant %s: %v", tenantID, err)
		http.Redirect(w, r, landingBase+"?instagram=error", http.StatusFound)
		return
	}

	log.Printf("crm: instagram oauth callback — tenant %s connected", tenantID)
	http.Redirect(w, r, landingBase+"?instagram=connected", http.StatusFound)
}

// requestScheme mirrors internal/slugreservation/redirect_handler.go's
// scheme-detection idiom: prefer X-Forwarded-Proto (set by the
// TLS-terminating proxy in front of production), falling back to
// r.TLS presence, defaulting to http for local/dev plaintext.
func requestScheme(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" || proto == "http" {
		scheme = proto
	}
	return scheme
}

var _ http.Handler = (*OAuthCallbackHandler)(nil)
