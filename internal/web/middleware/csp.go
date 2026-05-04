package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
)

// cspNonceCtxKey is the context key under which the CSP middleware stashes
// the per-request nonce. The unexported type prevents collisions across
// packages — only this package can read or write the value.
type cspNonceCtxKey struct{}

// cspHeaderTemplate is the canonical Content-Security-Policy template from
// ADR 0082 §1. The literal token "{nonce}" is replaced per request with the
// generated nonce. Caddy serves the same template to static assets; for HTML
// the Go middleware overwrites it because we own the nonce.
//
// Style-src keeps 'unsafe-inline' as documented technical debt (ADR 0082 §3,
// review target Q4 2026).
//
// Launch-readiness gates (SIN-62288 items 7 & 8):
//   - The "https://static.crm.exemplo.com" img-src host is a placeholder.
//     Replace with the production CDN host (or promote to a config knob)
//     before exiting beta.
//   - This template emits no `report-to` / `report-uri`. Wire one once a
//     reporting endpoint or third-party CSP collector is decided.
const cspHeaderTemplate = "default-src 'self'; " +
	"script-src 'self' 'nonce-{nonce}'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https://static.crm.exemplo.com; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"form-action 'self'; " +
	"base-uri 'self'; " +
	"object-src 'none'; " +
	"upgrade-insecure-requests"

// nonceBytes is the byte length of the random nonce material before
// base64url encoding. ADR 0082 §2 specifies 16 bytes (128 bits), which is
// the W3C-recommended floor and yields a 22-char URL-safe nonce.
const nonceBytes = 16

// CSPNonce returns the per-request CSP nonce stored by the CSP middleware,
// or the empty string when called outside a CSP-protected request. Template
// helpers should fail closed (omit the script tag) on the empty value
// rather than rendering an unattributed <script>.
func CSPNonce(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(cspNonceCtxKey{}).(string)
	return v
}

// CSP returns an http.Handler middleware that:
//
//   - Generates a fresh 16-byte nonce per request (crypto/rand, base64url).
//   - Stashes the nonce in r.Context() for templates via CSPNonce.
//   - Writes the Content-Security-Policy header with the nonce substituted
//     into the canonical template (ADR 0082 §1).
//   - Sets Cache-Control: no-store so an HTML response that bakes the
//     per-request nonce into a <script> tag cannot be cached by an
//     intermediary and replayed across users with a stale nonce. ADR 0082
//     defers most cache headers to Caddy, but the protection here is
//     defence-in-depth: as long as the CSP middleware runs the response
//     is uncacheable regardless of upstream config. SIN-62285 will add a
//     mirroring `Cache-Control: no-store` on Caddy for HTML routes; the
//     two together close the gap when Caddy is bypassed (direct hits to
//     the app port, internal probes, smoke checks).
//
// The middleware uses crypto/rand directly so a depleted entropy source
// surfaces as an error rather than a silently-weak nonce: if rand.Read
// fails we send a 500 and skip the protected handler.
func CSP() func(http.Handler) http.Handler {
	return cspWith(rand.Read)
}

// cspWith is the testable seam: it lets tests inject a deterministic random
// source and assert the substituted header without relying on entropy from
// the OS. Production calls CSP() which fixes randSource = rand.Read.
func cspWith(randSource func([]byte) (int, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, nonceBytes)
			if _, err := randSource(buf); err != nil {
				// Failing closed protects against shipping HTML with a
				// predictable nonce when the entropy source is broken.
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			nonce := base64.RawURLEncoding.EncodeToString(buf)
			header := strings.Replace(cspHeaderTemplate, "{nonce}", nonce, 1)
			w.Header().Set("Content-Security-Policy", header)
			// Per-request nonces void caching: a cached HTML body would
			// re-serve a stale nonce that no longer matches the header,
			// breaking script execution and (worse) blanking out the
			// XSS protection. See SIN-62288 item 6.
			w.Header().Set("Cache-Control", "no-store")

			ctx := context.WithValue(r.Context(), cspNonceCtxKey{}, nonce)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
