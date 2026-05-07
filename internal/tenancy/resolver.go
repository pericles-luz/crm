package tenancy

import (
	"context"
	"errors"
)

// Resolver maps an incoming HTTP host (acme.crm.local, acme.com.br, …) to
// the Tenant that should scope the request. Implementations live behind
// the port so tests and the middleware never depend on the database.
//
// Implementations MUST return ErrTenantNotFound — and only that error —
// when the host is not registered. Anything else (timeouts, transport
// errors, etc.) is treated as a server error by the middleware.
type Resolver interface {
	ResolveByHost(ctx context.Context, host string) (*Tenant, error)
}

// ErrTenantNotFound signals that no tenant matches the supplied host.
// Callers MUST translate this into a generic 404 response — leaking the
// fact that some subdomains DO resolve enables tenant enumeration.
var ErrTenantNotFound = errors.New("tenancy: tenant not found")
