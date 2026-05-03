// Package cache builds tenant-scoped redis keys for the AI panel.
//
// Implements ADR 0077 §3.4 (SIN-62225): every AI cache read or write must be
// keyed through TenantKey (per-tenant) or SystemKey (master role). The
// returned Key is an opaque value type whose underlying string can only be
// produced by constructors in this package, so callers cannot fabricate a
// key from arbitrary input and bypass tenant isolation.
//
// Lenses applied:
//   - Least privilege: master operations live in the disjoint "system:ai:"
//     namespace and cannot collide with tenant keys ("tenant:{id}:ai:...").
//   - Defense in depth: the opaque Key type prevents accidental string
//     concatenation; the aicache analyzer (internal/lint/aicache) prevents
//     direct redis.Client calls anywhere under internal/ai outside the
//     adapter, so port.Cache is the only legal access path.
//   - OWASP LLM06: closes cross-tenant cache poisoning of AI summaries.
package cache

import "errors"

// ErrEmptyTenant is returned when TenantKey is called with a blank tenant id.
var ErrEmptyTenant = errors.New("ai/cache: tenant id must not be empty")

// ErrSystemTenantMisuse is returned when TenantKey is called with the literal
// "system" identifier (master operations must use SystemKey) or the zero UUID
// (a sentinel-shaped value that must never become a tenant boundary).
var ErrSystemTenantMisuse = errors.New(
	"ai/cache: tenantID must not be the master 'system' identifier or the zero UUID; use SystemKey for master-role operations",
)

// ErrEmptyScope is returned when SystemKey is called with a blank scope.
var ErrEmptyScope = errors.New("ai/cache: system key scope must not be empty")

// ErrSystemScopeMisuse is returned when SystemKey is called with the literal
// "system" scope, which would produce a key shaped like
// "system:ai:system:..." and confuse the master namespace with itself.
var ErrSystemScopeMisuse = errors.New("ai/cache: system key scope must not be 'system'")

// ErrEmptyConv is returned when either constructor is called with a blank
// conversation id.
var ErrEmptyConv = errors.New("ai/cache: conversation id must not be empty")

// ErrEmptyUntilMsg is returned when either constructor is called with a blank
// until-message identifier; an empty until-message would produce an ambiguous
// key that bleeds across regenerations.
var ErrEmptyUntilMsg = errors.New("ai/cache: untilMsg must not be empty")

const (
	reservedSystemTenant = "system"
	zeroUUID             = "00000000-0000-0000-0000-000000000000"

	tenantPrefix = "tenant:"
	systemPrefix = "system:ai:"
	summaryInfix = ":ai:summary:"
)

// Key is an opaque, validated cache key. Values can only be constructed by
// TenantKey or SystemKey because the underlying field is unexported, so a raw
// string cannot be passed to the cache adapter without going through the
// constructors.
type Key struct {
	value string
}

// String returns the materialised redis key. The aicache analyzer ensures
// String is only consumed inside the redis adapter; outside callers reach
// redis through the port.Cache interface, which itself accepts only Key.
func (k Key) String() string { return k.value }

// IsZero reports whether the key is the zero value, i.e. it has not been
// produced by a constructor. Adapters use this as a defensive guard.
func (k Key) IsZero() bool { return k.value == "" }

// TenantKey returns a tenant-scoped AI summary key with the shape
// "tenant:{tenantID}:ai:summary:{conv}:{untilMsg}".
//
// Inputs are validated:
//   - tenantID must be non-empty (ErrEmptyTenant).
//   - tenantID must not equal "system" or the zero UUID
//     (ErrSystemTenantMisuse). Master operations belong in SystemKey.
//   - conv must be non-empty (ErrEmptyConv).
//   - untilMsg must be non-empty (ErrEmptyUntilMsg).
func TenantKey(tenantID, conv, untilMsg string) (Key, error) {
	if tenantID == "" {
		return Key{}, ErrEmptyTenant
	}
	if tenantID == reservedSystemTenant || tenantID == zeroUUID {
		return Key{}, ErrSystemTenantMisuse
	}
	if conv == "" {
		return Key{}, ErrEmptyConv
	}
	if untilMsg == "" {
		return Key{}, ErrEmptyUntilMsg
	}
	return Key{value: tenantPrefix + tenantID + summaryInfix + conv + ":" + untilMsg}, nil
}

// SystemKey returns a master-role AI cache key with the shape
// "system:ai:{scope}:{conv}:{untilMsg}". The "system:ai:" namespace is
// disjoint from the tenant namespace ("tenant:{id}:ai:..."), so master
// operations cannot collide with any tenant.
//
// Inputs are validated:
//   - scope must be non-empty (ErrEmptyScope) and must not be the literal
//     "system" (ErrSystemScopeMisuse).
//   - conv must be non-empty (ErrEmptyConv).
//   - untilMsg must be non-empty (ErrEmptyUntilMsg).
func SystemKey(scope, conv, untilMsg string) (Key, error) {
	if scope == "" {
		return Key{}, ErrEmptyScope
	}
	if scope == reservedSystemTenant {
		return Key{}, ErrSystemScopeMisuse
	}
	if conv == "" {
		return Key{}, ErrEmptyConv
	}
	if untilMsg == "" {
		return Key{}, ErrEmptyUntilMsg
	}
	return Key{value: systemPrefix + scope + ":" + conv + ":" + untilMsg}, nil
}
