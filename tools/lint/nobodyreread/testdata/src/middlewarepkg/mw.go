// Fixture: middleware-shaped functions (`func(http.Handler) http.Handler`)
// MUST NOT consume r.Body — even one read breaks /webhooks/* bit-exactness.
package middlewarepkg

import (
	"io"
	"net/http"
)

// Standard middleware adapter that consumes r.Body — flagged.
func bodyLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // want `middleware bodyLoggingMiddleware reads r.Body`
		next.ServeHTTP(w, r)
	})
}

// Middleware that wraps with MaxBytesReader is OK — it does not consume.
func sizeLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

// Middleware that does not touch the body at all — OK.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
