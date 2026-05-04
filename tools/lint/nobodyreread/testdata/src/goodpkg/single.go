// Fixture: every pattern in this file must be silent — single reads,
// MaxBytesReader wrap, body Close after a single read, and the override
// marker for an intentional second read.
package goodpkg

import (
	"io"
	"net/http"
)

// MaxBodyBytes mirrors the production handler's cap.
const MaxBodyBytes = 1 << 20

// Single read with a MaxBytesReader wrap — the canonical webhook handler
// shape from internal/adapter/transport/http/webhook_handler.go.
func handlerOK(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		return
	}
	_ = body
}

// MaxBytesReader alone is fine — it does not consume.
func handlerMaxBytesOnly(w http.ResponseWriter, r *http.Request) {
	_ = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	_ = w
}

// Body.Close after a single read is OK; Close is not a read.
func handlerReadThenClose(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	_ = r.Body.Close()
	_ = w
}

// Override marker silences an intentional second read. Real production code
// must explain *why* in the suppression text.
func handlerOverridden(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body) // nobodyreread:ok first read drained for replay logging
	_, _ = io.ReadAll(r.Body) // nobodyreread:ok second read is intentional retry
	_ = w
}

// Two reads in *different* nested funcs are scoped per-func and don't
// double-count under the same handler.
func handlerNestedScopes(w http.ResponseWriter, r *http.Request) {
	go func() {
		_, _ = io.ReadAll(r.Body)
	}()
	_ = r
	_ = w
}
