package main

// Instagram outbound wiring — OutboundChannel sender for the Instagram
// Direct channel, mirroring messenger_wire.go's buildMessengerOutboundEntry
// almost exactly.
//
// Called exactly once, from inbox_wire_real.go's combined outbound
// Router — NOT from assembleInstagramAdapter (inbound never needs it).
// Building it twice would register the instagram_send_* Prometheus
// collectors twice and panic at boot — the same class of bug fixed for
// WhatsApp (commit 258a78c) and guarded against for Messenger.

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/channel/dispatch"
	channelinstagram "github.com/pericles-luz/crm/internal/adapter/channel/instagram"
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/inbox"
)

// buildInstagramOutboundEntry builds the decorated (idempotent +
// rate-limited) Instagram sender WITHOUT wrapping it in a Router, so
// inbox_wire_real.go can merge it with the WhatsApp / Messenger /
// fake-customer entries into one combined Router built once per boot.
// ok=false means Instagram outbound is disabled (construction error) —
// the caller should omit the entry from any combined route map.
//
// Unlike the pre-Business-Login-OAuth design, an empty
// META_INSTAGRAM_GRAPH_TOKEN no longer disables the sender outright as
// long as Business Login for Instagram is configured (META_INSTAGRAM_APP_ID
// + META_INSTAGRAM_APP_SECRET, see cmd/server/instagram_oauth_wire.go): a tenant
// that connects via /settings/channels' "Conectar Instagram" supplies its
// own TenantConfig.AccessToken, resolved per-send by
// channelinstagram.Sender, and the global token becomes a legacy
// fallback for any tenant that hasn't (re)connected — see
// instagram_oauth_tokens (migration 0138) / InstagramOAuthTokenStore. An
// environment with NEITHER the global token NOR Business Login configured
// has no possible source of an Instagram token at all, so the sender
// stays disabled there — same zero-risk posture as before for any
// deployment that hasn't touched Instagram.
//
// This is the ONLY construction site for channelinstagram.Sender — do
// not also build one inside assembleInstagramAdapter (inbound doesn't
// need it); see the file doc comment for why a second construction site
// panics at boot.
func buildInstagramOutboundEntry(getenv func(string) string, pool *pgxpool.Pool, rdb *goredis.Client, flag *instagram.EnvFeatureFlag) (inbox.OutboundChannel, bool) {
	token := instagramOutboundGraphToken(getenv)
	if instagramOutboundSenderDisabled(getenv) {
		log.Printf("crm: instagram outbound sender disabled (no META_INSTAGRAM_GRAPH_TOKEN and Business Login OAuth not configured)")
		return nil, false
	}
	if token == "" {
		log.Printf("crm: instagram outbound sender — no global fallback token (META_INSTAGRAM_GRAPH_TOKEN unset); relying on per-tenant Business Login tokens")
	}
	tokenStore := pgstore.NewInstagramOAuthTokenStore(pool)
	lookup := channelinstagram.TenantConfigLookup(func(ctx context.Context, tenantID uuid.UUID) (channelinstagram.TenantConfig, error) {
		igBusinessID, err := instagramOutboundIGBusinessID(ctx, pool, tenantID)
		if err != nil {
			return channelinstagram.TenantConfig{}, err
		}
		on, flagErr := flag.Enabled(ctx, tenantID)
		if flagErr != nil {
			return channelinstagram.TenantConfig{}, flagErr
		}
		accessToken, _, _, tokErr := tokenStore.Get(ctx, tenantID)
		if tokErr != nil {
			return channelinstagram.TenantConfig{}, tokErr
		}
		return channelinstagram.TenantConfig{IGBusinessID: igBusinessID, Enabled: on, AccessToken: accessToken}, nil
	})
	sender, err := channelinstagram.New(token, lookup, prometheus.DefaultRegisterer)
	if err != nil {
		log.Printf("crm: instagram outbound sender disabled — %v", err)
		return nil, false
	}
	var limiter dispatch.RateLimiter
	if rdb != nil {
		limiter = rlredis.New(rdb, "instagram:out:")
	}
	log.Printf("crm: instagram outbound sender ready")
	var oc inbox.OutboundChannel = sender
	oc = dispatch.NewRateLimited(oc, limiter, time.Minute, instagramOutboundRateMaxPerMin(getenv), nil)
	oc = dispatch.NewIdempotent(oc, dispatch.NewMemoryLedger())
	return oc, true
}

// envInstagramGraphToken holds the Instagram User access token obtained
// via Business Login for Instagram (instagram.com/oauth/authorize →
// api.instagram.com/oauth/access_token → the 60-day long-lived
// exchange at graph.instagram.com/access_token — see
// internal/adapter/channel/instagram/sender.go's package doc comment).
// Unlike Messenger/WhatsApp, this is NOT a Business Manager System
// User / Page Access Token and there is deliberately no fallback to
// the shared META_GRAPH_TOKEN — that token belongs to a different auth
// family (graph.facebook.com) and does not work against
// graph.instagram.com.
const envInstagramGraphToken = "META_INSTAGRAM_GRAPH_TOKEN"

// envInstagramAppID is the Meta App's OAuth client_id for Business Login
// for Instagram (see cmd/server/instagram_oauth_wire.go). Shared with
// instagram.EnvInstagramAppSecret ("META_INSTAGRAM_APP_SECRET", already
// used for webhook signature verification) as the two preconditions for
// buildInstagramOutboundEntry to construct the sender when no global
// fallback token is set.
const envInstagramAppID = "META_INSTAGRAM_APP_ID"

// instagramOutboundGraphToken reads META_INSTAGRAM_GRAPH_TOKEN.
func instagramOutboundGraphToken(getenv func(string) string) string {
	return strings.TrimSpace(getenv(envInstagramGraphToken))
}

// instagramOutboundSenderDisabled reports whether Instagram outbound has
// no possible token source at all: no global fallback AND Business Login
// for Instagram isn't configured (so no tenant could ever obtain a
// per-tenant token either). Split out as a pure function so the gating
// logic is unit-testable without invoking channelinstagram.New — that
// call registers Prometheus collectors on the process-wide
// prometheus.DefaultRegisterer, and a second registration anywhere else
// in the test binary would panic (see the file doc comment).
func instagramOutboundSenderDisabled(getenv func(string) string) bool {
	token := instagramOutboundGraphToken(getenv)
	oauthConfigured := strings.TrimSpace(getenv(envInstagramAppID)) != "" && strings.TrimSpace(getenv(instagram.EnvInstagramAppSecret)) != ""
	return token == "" && !oauthConfigured
}

// defaultInstagramRateMaxPerMin mirrors defaultMessengerRateMaxPerMin
// for the Instagram outbound channel.
const defaultInstagramRateMaxPerMin = 600

// envInstagramOutboundRateMaxPerMin is DELIBERATELY distinct from
// instagram.EnvInstagramRateMax ("INSTAGRAM_RATE_MAX_PER_MIN"), which
// already governs the INBOUND per-ig_business_id webhook rate limiter
// (internal/adapter/channels/instagram/config.go). Reusing that name
// here would silently couple two independent budgets (inbound flood
// protection vs. outbound send throughput) that operators need to tune
// separately.
const envInstagramOutboundRateMaxPerMin = "INSTAGRAM_OUTBOUND_RATE_MAX_PER_MIN"

// instagramOutboundRateMaxPerMin reads envInstagramOutboundRateMaxPerMin,
// falling back to defaultInstagramRateMaxPerMin for an unset or
// non-positive value.
func instagramOutboundRateMaxPerMin(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(envInstagramOutboundRateMaxPerMin))
	if raw == "" {
		return defaultInstagramRateMaxPerMin
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultInstagramRateMaxPerMin
	}
	return n
}

// instagramOutboundIGBusinessID returns the Instagram Business Account
// id associated with a tenant for outbound sends. It queries
// tenant_channel_associations with channel="instagram" and tenant_id=$1
// (reverse of the inbound direction) — the same table + row the inbound
// resolver reads in the opposite direction, populated by
// /settings/channels via ChannelAssociationWriter.SaveAssociation.
//
// When no row exists the tenant has not configured an Instagram
// account; the caller receives ErrChannelAuthFailed (via
// instagramOutboundIGBusinessID → TenantConfig.IGBusinessID=="").
func instagramOutboundIGBusinessID(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (string, error) {
	const sql = `
		SELECT association
		  FROM tenant_channel_associations
		 WHERE channel = $1 AND tenant_id = $2
		 LIMIT 1`
	var igBusinessID string
	err := pool.QueryRow(ctx, sql, instagram.Channel, tenantID).Scan(&igBusinessID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // caller surfaces as ErrChannelAuthFailed via empty IGBusinessID
	}
	if err != nil {
		return "", err
	}
	return igBusinessID, nil
}
