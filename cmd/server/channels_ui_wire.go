package main

// SIN-66391 (P2) wiring — HTMX channel-management admin UI. Mounts the
// routes under /settings/channels backed by the SIN-66389 channels
// Postgres adapter (channels.Repository + channels.AccessRepository).
//
// Only the runtime pool (app_runtime, RLS-gated) is needed: the
// channel_access master-ops audit trigger is a no-op for app_runtime
// (current_user <> 'app_master_ops' → RETURN NEW), so the roster writes
// route through the same pool as the reads. When DATABASE_URL is unset or
// unreachable the wire returns a nil handler and the router leaves the
// /settings/channels routes unmounted — the same fail-soft pattern as the
// other web/* wires.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	channelinstagram "github.com/pericles-luz/crm/internal/adapter/channel/instagram"
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgchannels "github.com/pericles-luz/crm/internal/adapter/db/postgres/channels"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/channels"
	webchannels "github.com/pericles-luz/crm/internal/web/channels"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// buildWebChannelsHandler returns the channel-management admin mux + a
// cleanup closure that releases the pgxpool the wire opened. A nil handler
// signals "skip mounting on the public listener" so callers can defer the
// cleanup unconditionally.
func buildWebChannelsHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	if getenv(pgpool.EnvDSN) == "" {
		log.Printf("crm: web/channels disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/channels disabled — pg runtime connect: %v", err)
		return nil, noop
	}
	store, err := pgchannels.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/channels disabled — channels store: %v", err)
		return nil, noop
	}
	// The user-label directory is best-effort chrome: a failure degrades
	// the app-shell account label to the shell fallback ("Conta"), it does
	// not disable the surface.
	var userDir userlabel.Directory
	if dir, derr := pginbox.NewUserDirectory(pool); derr != nil {
		log.Printf("crm: web/channels — user directory unavailable, using shell fallback: %v", derr)
	} else {
		userDir = dir
	}
	// Channel access-change events (grant / revoke / restricted-flip) are
	// privilege events that must land in audit_log_security (SIN-66405).
	// Route them through the same SplitLogger the other privilege events
	// use, on the RLS-gated runtime pool. A logger-build failure degrades
	// to no audit emission rather than disabling the surface — the writes
	// still commit; only the trail is best-effort.
	var auditor webchannels.AccessAuditor
	if splitLogger, aerr := pgpool.NewSplitAuditLogger(pool); aerr != nil {
		log.Printf("crm: web/channels — access audit disabled: %v", aerr)
	} else {
		auditor = newChannelAccessAuditor(splitLogger, slog.Default())
	}
	// WhatsApp API onboarding (SIN-67143): the association writer persists a
	// registered channel's phone_number_id → tenant mapping on the same
	// RLS-gated runtime pool, so the inbound webhook entry-point can resolve
	// the tenant. Mirrors the NewChannelAssociationLookup wiring on the read
	// side.
	associations := pgstore.NewChannelAssociationWriter(pool)
	// "Conectar Instagram" (Business Login for Instagram self-service):
	// wired only when the same two env vars buildInstagramOAuthWiring
	// requires are set — otherwise the button stays hidden rather than
	// offering a dead-end link (the callback route wouldn't be mounted
	// either). Reuses this handler's own pool for the Status() lookup.
	instagramOAuth := buildInstagramConnector(pool, getenv)
	handler, err := assembleWebChannelsHandlerWithDeps(store, userDir, slog.Default(), whatsappWebEnabled(getenv), fakeCustomerChannelEnabled(getenv), associations, instagramOAuth, auditor)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/channels disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/channels HTMX routes mounted (channels adapter wired)")
	return handler, func() { pool.Close() }
}

// channelsStore unions the ports the wire consumes from the pgx adapter.
// Declared at the composition root so the test can swap in an in-memory
// fake without importing pgx.
type channelsStore interface {
	channels.Repository
	channels.AccessRepository
}

// assembleWebChannelsHandler is the pure assembly seam. Tests call it
// directly with a stub store so the wire is exercised without booting the
// whole server.
// The variadic auditor keeps the pre-SIN-66405 3-arg call sites (tests)
// source-compatible while letting the composition root inject the access
// auditor as a 4th argument. Only the first auditor is used; extra args
// are ignored.
func assembleWebChannelsHandler(store channelsStore, userLabels userlabel.Directory, logger *slog.Logger, auditor ...webchannels.AccessAuditor) (http.Handler, error) {
	return assembleWebChannelsHandlerFlagged(store, userLabels, logger, false, auditor...)
}

// buildInstagramConnector wires the "Conectar Instagram" self-service
// glue when Business Login for Instagram is configured (the same
// META_INSTAGRAM_APP_ID + META_INSTAGRAM_APP_SECRET precondition
// buildInstagramOAuthWiring checks) — nil otherwise, hiding the button.
func buildInstagramConnector(pool instagramConnectorPool, getenv func(string) string) webchannels.InstagramConnector {
	appID := strings.TrimSpace(getenv(envInstagramAppID))
	appSecret := strings.TrimSpace(getenv(instagram.EnvInstagramAppSecret))
	if appID == "" || appSecret == "" {
		return nil
	}
	return &instagramConnectorGlue{
		cfg:         channelinstagram.OAuthConfig{AppID: appID, AppSecret: appSecret},
		stateSecret: []byte(appSecret),
		store:       pgstore.NewInstagramOAuthTokenStore(pool),
	}
}

// instagramConnectorPool is the narrow pgstore.PgxConn surface
// buildInstagramConnector needs — declared locally so tests can pass a
// stub without importing pgx.
type instagramConnectorPool = pgstore.PgxConn

// instagramConnectorGlue implements webchannels.InstagramConnector by
// composing the OAuth mechanics (state signing + authorize URL) with the
// token store's Status lookup. Pure composition-root glue: no logic here
// beyond what each collaborator already owns.
type instagramConnectorGlue struct {
	cfg         channelinstagram.OAuthConfig
	stateSecret []byte
	store       *pgstore.InstagramOAuthTokenStore
}

func (g *instagramConnectorGlue) AuthorizeURL(tenantID uuid.UUID, redirectURI string) (string, error) {
	state, err := channelinstagram.SignOAuthState(g.stateSecret, tenantID, 10*time.Minute)
	if err != nil {
		return "", fmt.Errorf("sign instagram oauth state: %w", err)
	}
	return g.cfg.AuthorizeURL(redirectURI, state), nil
}

func (g *instagramConnectorGlue) Status(ctx context.Context, tenantID uuid.UUID) (bool, time.Time, error) {
	_, expiresAt, ok, err := g.store.Get(ctx, tenantID)
	return ok, expiresAt, err
}

// Compile-time assertion that instagramConnectorGlue satisfies the web
// handler's InstagramConnector port.
var _ webchannels.InstagramConnector = (*instagramConnectorGlue)(nil)

// EnvWhatsAppWebEnabled gates the "WhatsApp Web" (unofficial QR-session)
// channel type. Default OFF; set to "1" to offer + accept the type in a
// tenant's create form. Mirrors the FEATURE_WEBCHAT_ENABLED convention.
const EnvWhatsAppWebEnabled = "FEATURE_WHATSAPP_WEB_ENABLED"

// whatsappWebEnabled reads the WhatsApp-Web feature flag from the
// environment at boot (input, not side-effect in the web core).
func whatsappWebEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	return strings.TrimSpace(getenv(EnvWhatsAppWebEnabled)) == "1"
}

// fakeCustomerChannelEnabled reads INBOX_FAKE_CUSTOMER_ENABLED (defined in
// inbox_fake_customer_wire.go) from the boot environment, gating the
// FUNCTIONAL readiness of the "Cliente Fake (Demo)" channel type the same
// way whatsappWebEnabled gates WhatsApp Web.
func fakeCustomerChannelEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	return strings.TrimSpace(getenv(envInboxFakeCustomerEnabled)) == "1"
}

// assembleWebChannelsHandlerFlagged is the flag-aware assembly seam. The
// wsWebEnabled input threads FEATURE_WHATSAPP_WEB_ENABLED from the boot
// environment into the handler's WhatsAppWebEnabled field. It delegates to
// assembleWebChannelsHandlerWithDeps with no association writer (nil ⇒
// WhatsApp-API onboarding skipped) and fake-customer readiness off, keeping
// the pre-SIN-67143 call sites source-compatible.
func assembleWebChannelsHandlerFlagged(store channelsStore, userLabels userlabel.Directory, logger *slog.Logger, wsWebEnabled bool, auditor ...webchannels.AccessAuditor) (http.Handler, error) {
	return assembleWebChannelsHandlerWithDeps(store, userLabels, logger, wsWebEnabled, false, nil, nil, auditor...)
}

// assembleWebChannelsHandlerWithDeps is the deepest assembly seam. It adds
// the optional WhatsApp-API association writer (SIN-67143), the
// fake-customer channel readiness flag, and the optional "Conectar
// Instagram" connector on top of the flag-aware seam; a nil writer
// disables WhatsApp-API onboarding and a nil connector hides the
// Instagram button, so the surface still renders + creates channels
// under fail-soft wiring either way.
func assembleWebChannelsHandlerWithDeps(store channelsStore, userLabels userlabel.Directory, logger *slog.Logger, wsWebEnabled bool, fakeCustomerEnabled bool, associations webchannels.ChannelAssociationWriter, instagramOAuth webchannels.InstagramConnector, auditor ...webchannels.AccessAuditor) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("channels_wire: store is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var accessAuditor webchannels.AccessAuditor
	if len(auditor) > 0 {
		accessAuditor = auditor[0]
	}
	h, err := webchannels.New(webchannels.Deps{
		Channels:            store,
		Access:              store,
		CSRFToken:           csrfTokenFromSessionContext,
		UserID:              userIDFromSessionContext,
		UserLabels:          userLabels,
		Audit:               accessAuditor,
		Logger:              logger,
		WhatsAppWebEnabled:  wsWebEnabled,
		FakeCustomerEnabled: fakeCustomerEnabled,
		Associations:        associations,
		InstagramOAuth:      instagramOAuth,
	})
	if err != nil {
		return nil, fmt.Errorf("channels_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// Compile-time guard: the pgx adapter satisfies the channelsStore union.
var _ channelsStore = (*pgchannels.Store)(nil)

// Compile-time guard: the postgres association writer satisfies the web
// handler's ChannelAssociationWriter port (SIN-67143).
var _ webchannels.ChannelAssociationWriter = (*pgstore.ChannelAssociationWriter)(nil)
