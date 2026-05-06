// Fixture: a package that is neither under internal/webhook nor under
// internal/adapter. The analyzer must not police it at all.
package goodpkg

import (
	"log"
	"log/slog"
)

// LogPlainSecretsOutsideScope intentionally logs every secret label — the
// analyzer must stay silent because this package's import path does not
// match any of the policed substrings.
func LogPlainSecretsOutsideScope() {
	log.Printf("webhook_token=%s raw_payload=%s Authorization=%s", "a", "b", "c")
	slog.Info("debug", "tenant_id", "abc", "tenant_slug", "acme")
}
