package middleware

import (
	"context"

	"github.com/pericles-luz/crm/internal/iam"
)

// decisionCtxKey is the unexported context-key type for the Authorizer
// decision attached by RequireAction. Downstream audit middleware
// ([SIN-62254]) pulls it out via DecisionFromContext.
type decisionCtxKey struct{}

// WithDecision attaches d to ctx. RequireAction calls this once per
// request after consulting the Authorizer.
func WithDecision(ctx context.Context, d iam.Decision) context.Context {
	return context.WithValue(ctx, decisionCtxKey{}, d)
}

// DecisionFromContext returns the decision injected by RequireAction.
// The bool is false when RequireAction was not on the chain — audit
// writers SHOULD treat that as "no authz applied" rather than allow.
func DecisionFromContext(ctx context.Context) (iam.Decision, bool) {
	d, ok := ctx.Value(decisionCtxKey{}).(iam.Decision)
	return d, ok
}
