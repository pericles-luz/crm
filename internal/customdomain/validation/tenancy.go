package validation

import (
	"context"

	"github.com/google/uuid"
)

// tenantIDCtxKey is the unexported context-key type used to plumb the
// tenant id from the wire-up adapters into the Validator without
// changing its public API. Wire-up code calls WithTenantID before
// invoking Validate / ValidateHostOnly; the validator pulls the id back
// out for the LogEntry it sends to Writer.
type tenantIDCtxKey struct{}

// WithTenantID returns a copy of ctx carrying tenantID. The wire-up
// adapter that owns the request context is responsible for calling this
// before forwarding into the validator. Passing uuid.Nil is allowed and
// is treated by TenantIDFromContext as "no tenant attached".
func WithTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDCtxKey{}, tenantID)
}

// TenantIDFromContext returns the tenant id stored on ctx, or uuid.Nil
// if no tenant was attached. The validator persists uuid.Nil as NULL
// so forensics can tell anonymous calls apart from tenant-scoped ones.
func TenantIDFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(tenantIDCtxKey{}).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}
