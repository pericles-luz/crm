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

// IGBusinessIDLookup resolves a tenant's Instagram Business Account id
// (the identity entered at /settings/channels create time, stored in
// tenant_channel_associations) — needed after a successful token
// exchange to call SubscribeApp. Returns "" with a nil error when the
// tenant has no association yet.
type IGBusinessIDLookup interface {
	IGBusinessID(ctx context.Context, tenantID uuid.UUID) (string, error)
}

// OAuthCallbackHandler exchanges the authorization code for a long-lived
// token, stores it, and subscribes the account to this app's webhook.
// Implements http.Handler.
type OAuthCallbackHandler struct {
	cfg         OAuthConfig
	stateSecret []byte
	store       OAuthTokenSaver
	igLookup    IGBusinessIDLookup
}

// NewOAuthCallbackHandler builds an OAuthCallbackHandler. All arguments
// are required.
func NewOAuthCallbackHandler(cfg OAuthConfig, stateSecret []byte, store OAuthTokenSaver, igLookup IGBusinessIDLookup) *OAuthCallbackHandler {
	return &OAuthCallbackHandler{cfg: cfg, stateSecret: stateSecret, store: store, igLookup: igLookup}
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

	// Subscribing the account to this app's webhook is best-effort here:
	// the token already saved above is enough for outbound sending to
	// work, so a subscribe failure degrades (no inbound delivery until a
	// retry) rather than bouncing an otherwise-successful connect.
	igBusinessID, err := h.igLookup.IGBusinessID(r.Context(), tenantID)
	if err != nil || igBusinessID == "" {
		log.Printf("crm: instagram oauth callback — tenant %s connected but ig_business_id not found, cannot subscribe app for inbound webhooks: %v", tenantID, err)
	} else if err := h.cfg.SubscribeApp(r.Context(), igBusinessID, accessToken); err != nil {
		log.Printf("crm: instagram oauth callback — subscribe app failed for tenant %s: %v", tenantID, err)
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
