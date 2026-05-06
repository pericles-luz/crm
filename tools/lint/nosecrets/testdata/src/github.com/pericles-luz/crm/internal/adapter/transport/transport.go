// Fixture: an adapter transport package that imports webhook. The
// always-forbidden list applies; pre-HMAC tenant rule applies because the
// file imports webhook.
package transport

import (
	"fmt"
	"log/slog"

	"github.com/pericles-luz/crm/internal/webhook"
)

// fmtErrorfWithSecretIsForbidden — fmt.Errorf is treated as a log call (the
// returned error commonly ends up in a logger).
func fmtErrorfWithSecretIsForbidden() error {
	return fmt.Errorf("dispatch failed: webhook_token=%s", "x") // want `forbidden field "webhook_token" in webhook scope`
}

// preHMACTenantSlugInAdapterIsForbidden — pre-HMAC tenant_slug also fires.
func preHMACTenantSlugInAdapterIsForbidden(a webhook.Adapter) {
	slog.Debug("dispatch", "tenant_slug", "acme") // want `forbidden field "tenant_slug" in pre-HMAC scope`
	_ = a
}

// methodOnLoggerIsAlsoChecked — methods on *slog.Logger / *log.Logger are
// also considered log calls.
func methodOnLoggerIsAlsoChecked(l *slog.Logger) {
	l.Info("dispatch", "raw_payload", "{}") // want `forbidden field "raw_payload" in webhook scope`
}
