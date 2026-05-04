// Fixture: a webhook-package handler. Pre-HMAC log lines must not include
// tenant_id / tenant_slug; secret labels are always forbidden. Calls after
// VerifyApp are post-HMAC and may carry tenant_id.
package handler

import (
	"context"
	"log"
	"log/slog"

	"github.com/pericles-luz/crm/internal/webhook"
)

// preHMACTenantLogIsForbidden — the tenant_id label appears BEFORE the
// gate, so F-9 fires.
func preHMACTenantLogIsForbidden(ctx context.Context, a webhook.Adapter, body []byte) {
	log.Printf("incoming webhook tenant_id=%s", "abc") // want `forbidden field "tenant_id" in pre-HMAC scope`
	_ = a.VerifyApp(ctx, body)
}

// preHMACSecretLogIsForbidden — `webhook_token` and `raw_payload` are always
// forbidden, regardless of pre/post HMAC.
func preHMACSecretLogIsForbidden(ctx context.Context, a webhook.Adapter, body []byte) {
	log.Printf("token=%s", "webhook_token") // want `forbidden field "webhook_token" in webhook scope`
	_ = a.VerifyApp(ctx, body)
	slog.Info("event captured", "raw_payload", body) // want `forbidden field "raw_payload" in webhook scope`
}

// authorizationHeaderInLogIsForbidden — Authorization header value MUST
// never reach a logger.
func authorizationHeaderInLogIsForbidden(ctx context.Context, a webhook.Adapter, body []byte) {
	_ = a.VerifyApp(ctx, body)
	slog.Warn("debug", "Authorization", "Bearer redacted-but-still-bad") // want `forbidden field "Authorization" in webhook scope`
}

// postHMACTenantIsAllowed — tenant_id appears AFTER VerifyApp returns; the
// analyzer treats it as authenticated and stays silent.
func postHMACTenantIsAllowed(ctx context.Context, a webhook.Adapter, body []byte) {
	_ = a.VerifyApp(ctx, body)
	slog.Info("authenticated webhook", "tenant_id", "abc")
}

// overrideMarkerSilencesPreHMACTenant — explicit override per ADR §5.
func overrideMarkerSilencesPreHMACTenant(ctx context.Context, a webhook.Adapter, body []byte) {
	// nosecrets:ok intentionally logging the claimed tenant for forensics
	log.Printf("claim tenant_id=%s for forensics", "abc")
	_ = a.VerifyApp(ctx, body)
}
