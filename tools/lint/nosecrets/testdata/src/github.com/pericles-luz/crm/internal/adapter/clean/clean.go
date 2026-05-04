// Fixture: an adapter package that does not import webhook. The
// always-forbidden list applies; the pre-HMAC tenant rule does NOT apply
// (no webhook import). Logs of tenant_id / tenant_slug are silent here —
// these adapters are not on the webhook path.
package clean

import "log/slog"

func tenantSlugWithoutWebhookImportIsAllowed(l *slog.Logger) {
	l.Info("dispatch", "tenant_slug", "acme")
	l.Info("dispatch", "tenant_id", "abc")
}

func benignLogIsAllowed() {
	slog.Info("ok", "request_id", "rid-123", "channel", "meta_cloud")
}
