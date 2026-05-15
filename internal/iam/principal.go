package iam

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Principal is the subject of an authorization decision. It is derived
// from an authenticated iam.Session at the HTTP boundary and carried in
// the request context. Handlers and the Authorizer consume it; raw
// Session values do NOT cross the authz seam.
//
// MasterImpersonating is true when a master operator is acting "as"
// a tenant — e.g. via the impersonation middleware. The Authorizer
// treats this as a heightened-risk principal for PII-sensitive actions
// (ADR 0090 §Master tenant-PII gate).
//
// MFAVerifiedAt mirrors mastermfa.Session.MFAVerifiedAt for the active
// session. A nil/zero value means the principal has not completed an
// MFA step-up; the Authorizer's PII gate consults this freshness to
// allow or deny PII reads while impersonating.
//
// Roles is a SET (deduplicated) of iam.Role values the principal carries
// for this request. The default RBAC matrix derives required-role lists
// from this set; an empty set means "no role" → deny by default.
type Principal struct {
	UserID              uuid.UUID
	TenantID            uuid.UUID
	Roles               []Role
	MasterImpersonating bool
	MFAVerifiedAt       *time.Time
}

// HasRole reports whether p carries the given role. The check is exact
// on the persisted role string (case-sensitive) — Role.Valid() should
// gate the construction site, not this comparison.
func (p Principal) HasRole(r Role) bool {
	for _, have := range p.Roles {
		if have == r {
			return true
		}
	}
	return false
}

// IsMaster reports whether p carries RoleMaster. Used by the Authorizer
// to short-circuit master-only actions and by the PII gate to identify
// the impersonation path.
func (p Principal) IsMaster() bool { return p.HasRole(RoleMaster) }

// PrincipalFromSession constructs a Principal from an authenticated
// iam.Session. The MasterImpersonating bit and MFAVerifiedAt come from
// adjacent middleware state — Session itself does not carry them, so
// the caller MUST resolve them before calling this. The function is a
// helper, not a security gate; it does NOT validate Session.
func PrincipalFromSession(s Session, masterImpersonating bool, mfaVerifiedAt *time.Time) Principal {
	return Principal{
		UserID:              s.UserID,
		TenantID:            s.TenantID,
		Roles:               []Role{s.Role},
		MasterImpersonating: masterImpersonating,
		MFAVerifiedAt:       mfaVerifiedAt,
	}
}

// principalCtxKey is the unexported context-key type for the resolved
// Principal. Kept in iam so any package that consumes a Principal must
// route through PrincipalFromContext rather than reach for the raw key.
type principalCtxKey struct{}

// WithPrincipal attaches p to ctx for downstream handlers. The HTTP
// middleware (RequireAuth) calls this once after deriving Principal
// from the session.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the principal injected by RequireAuth.
// The bool is false when the request never went through RequireAuth —
// callers SHOULD treat that as a programmer error and fail closed.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}
