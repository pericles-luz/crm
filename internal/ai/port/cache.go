// Package port declares the hexagonal ports for the AI bounded context.
//
// Ports here describe pure-domain interfaces; concrete adapters live under
// internal/ai/adapter/*. ADR 0077 §3.8 places the cache port in the domain so
// that storage technology (redis today, anything tomorrow) can be swapped
// without touching the AI summariser or the prompt builder.
package port

import (
	"context"
	"errors"
	"time"

	"github.com/pericles-luz/crm/internal/ai/cache"
)

// ErrCacheMiss signals that the requested key has no entry. Adapters MUST
// return this sentinel (wrapping is permitted) on lookup misses so that
// callers can distinguish a miss from an infrastructure failure.
var ErrCacheMiss = errors.New("ai/port: cache miss")

// Cache is the port through which the AI bounded context reads and writes
// per-tenant or master-role cached values.
//
// The key argument is the opaque cache.Key built by cache.TenantKey or
// cache.SystemKey. Accepting cache.Key (and not string) is the compile-time
// guard required by ADR 0077 §3.4: there is no way for a caller to pass a
// raw string into a Cache implementation, so tenant scoping cannot be
// bypassed at the call site.
type Cache interface {
	// Get returns the cached value for key, or ErrCacheMiss if no value is
	// stored. The returned slice is owned by the caller.
	Get(ctx context.Context, key cache.Key) ([]byte, error)

	// Set stores value under key with the given TTL. A zero or negative ttl
	// disables expiration; adapters MAY clamp this to a maximum to protect
	// memory budgets.
	Set(ctx context.Context, key cache.Key, value []byte, ttl time.Duration) error

	// Del removes key. Deleting a missing key MUST NOT return an error so that
	// idempotent regenerate flows do not have to distinguish absent keys.
	Del(ctx context.Context, key cache.Key) error
}
