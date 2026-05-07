package serve

import "net/http"

// staticOriginCSP is the Content-Security-Policy applied to every
// response from the cookieless static origin. It is intentionally the
// most restrictive policy the responses can support:
//
//   - default-src 'none'  — block scripts, plugins, frames, fonts, …
//   - img-src 'self'      — only same-origin images load (the static
//     origin only ever serves images and PDFs).
//   - style-src 'unsafe-inline' — Caddy's default error pages emit a
//     small inline <style>; whitelisting `unsafe-inline` here is bounded
//     because there is no script context to abuse it from.
//
// CSP belongs in middleware (not the handler) because Caddy may also
// emit error responses for paths that never reach Go (e.g. malformed
// HTTP). Middleware ensures every byte that traverses the static origin
// carries the same baseline.
const staticOriginCSP = "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'"

// MediaHeaders sets the static-origin defense-in-depth headers on every
// response: nosniff (kills MIME-confusion attacks even if a polyglot
// somehow slips past the upload re-encoder), Vary on Origin (so
// CORS-aware caches do not collapse cross-origin variants), the
// restrictive CSP above, and Cross-Origin-Resource-Policy: same-origin
// (so a cross-origin embedder cannot hot-link tenant assets and read
// their pixel data via canvas — see ADR 0080 §6 + SIN-62330).
// Per-resource Cache-Control and Content-Disposition are NOT set here
// — they are the handler's job because they vary by route (logo vs.
// content-addressed) and resource (image vs. PDF).
//
// Headers are written before delegating to the inner handler so that
// the inner handler can override Cache-Control without fighting the
// middleware.
func MediaHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Vary", "Origin")
		h.Set("Content-Security-Policy", staticOriginCSP)
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
