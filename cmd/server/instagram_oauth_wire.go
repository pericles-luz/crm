package main

// Instagram Business Login OAuth callback wiring — the public-mux
// receiver for the redirect Instagram sends the operator's browser back
// to after /settings/channels' "Conectar Instagram" (see
// internal/web/channels/handler.go's connectInstagram and
// cmd/server/channels_ui_wire.go's instagramConnectorGlue for the other
// end of this flow). Mirrors instagram_wire.go's shape (own pool, own
// Cleanup, fail-soft on missing config), but is otherwise independent:
// this callback is stateless (the tenant is resolved from the signed
// `state` query param — see
// internal/adapter/channel/instagram/oauth_state.go), so it shares no
// wiring with the inbound webhook adapter.

import (
	"context"
	"log"
	"net/http"
	"strings"

	channelinstagram "github.com/pericles-luz/crm/internal/adapter/channel/instagram"
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

// instagramOAuthWiring bundles the artifacts buildInstagramOAuthWiring
// produces. Register mounts GET <channelinstagram.OAuthCallbackPath> on
// a stdlib mux; Cleanup releases the pgxpool the wire opened.
type instagramOAuthWiring struct {
	Register func(*http.ServeMux)
	Cleanup  func()
}

// buildInstagramOAuthWiring assembles the Business Login for Instagram
// OAuth callback receiver. Returns nil when META_INSTAGRAM_APP_ID or
// META_APP_SECRET is unset, or DATABASE_URL is unset/unreachable — the
// caller treats nil as "skip mounting the OAuth callback route" (the
// "Conectar Instagram" button is correspondingly hidden by
// channels_ui_wire.go's instagramConnectorGlue under the same env-var
// precondition, so operators never see a dead-end link).
func buildInstagramOAuthWiring(ctx context.Context, getenv func(string) string) *instagramOAuthWiring {
	appID := strings.TrimSpace(getenv(envInstagramAppID))
	appSecret := strings.TrimSpace(getenv(instagram.EnvAppSecret))
	if appID == "" || appSecret == "" {
		log.Printf("crm: instagram oauth callback disabled (META_INSTAGRAM_APP_ID / META_APP_SECRET unset)")
		return nil
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: instagram oauth callback disabled (DATABASE_URL unset)")
		return nil
	}
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		log.Printf("crm: instagram oauth callback disabled — pg connect: %v", err)
		return nil
	}
	store := pgstore.NewInstagramOAuthTokenStore(pool)
	igLookup := pgstore.NewInstagramIGBusinessIDLookup(pool)
	cfg := channelinstagram.OAuthConfig{AppID: appID, AppSecret: appSecret}
	handler := channelinstagram.NewOAuthCallbackHandler(cfg, []byte(appSecret), store, igLookup)

	register := func(mux *http.ServeMux) {
		mux.Handle("GET "+channelinstagram.OAuthCallbackPath, handler)
	}
	log.Printf("crm: instagram oauth callback mounted on public listener")
	return &instagramOAuthWiring{Register: register, Cleanup: func() { pool.Close() }}
}
