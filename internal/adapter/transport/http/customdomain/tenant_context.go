// Package customdomain hosts the HTTP boundary for the tenant-facing
// custom-domain management UI (SIN-62259). It renders server-side HTML
// via html/template and uses HTMX for partial swaps.
//
// The full session/cookie auth flow is owned by a separate ticket. This
// package consumes a tenant ID from the request context (deny-by-default
// when absent) and lets cmd/server inject either the production
// middleware or, for the dev/feature-flag UI, a header-driven stub that
// reads `X-Tenant-ID` and verifies the request carries a matching CSRF
// token.
package customdomain

import (
	"context"

	"github.com/google/uuid"
)

// ctxKey is the unexported type used as a map key in request contexts —
// avoids collisions with other packages that store values under string
// keys.
type ctxKey int

const (
	ctxKeyTenantID ctxKey = iota
)

// WithTenantID returns a copy of ctx carrying tenantID. Auth middleware
// should call it after validating the session/JWT cookie.
func WithTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyTenantID, tenantID)
}

// TenantIDFromContext returns the tenant ID stored on ctx, or uuid.Nil
// if no tenant was attached. Callers MUST treat uuid.Nil as a 401/403.
func TenantIDFromContext(ctx context.Context) uuid.UUID {
	v, ok := ctx.Value(ctxKeyTenantID).(uuid.UUID)
	if !ok {
		return uuid.Nil
	}
	return v
}
