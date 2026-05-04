// Fixture: a webhook-scoped package whose only random source is
// crypto/rand stays silent — the analyzer policed the file and
// found nothing forbidden.
//
// NOTE: this fixture intentionally lives at a top-level "webhookpkg"
// path that DOES NOT contain the configured webhook substring; the
// real webhook fixture is under .../internal/webhook/. We keep this
// file to prove the analyzer skips packages that match neither rule.
package webhookpkg

import (
	"crypto/rand"
)

func Bytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
