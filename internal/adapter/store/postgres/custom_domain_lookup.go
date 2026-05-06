package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

// TLSAskLookup is the read-only Repository implementation the Caddy
// on_demand_tls.ask handler consults (SIN-62243 F45). It runs a single
// indexed SELECT against tenant_custom_domains; the index hit is the
// case-insensitive partial unique index uq_tenant_custom_domains_active_host
// declared in 0010_tenant_custom_domains.up.sql.
type TLSAskLookup struct {
	db PgxConn
}

// NewTLSAskLookup binds the lookup to the given pgx pool/conn. Pass the
// same *pgxpool.Pool the rest of the read paths use.
func NewTLSAskLookup(db PgxConn) *TLSAskLookup { return &TLSAskLookup{db: db} }

// tlsAskLookupSQL: the active row for a host is the most-recent
// (deleted_at IS NULL, lower(host) = lower($1)) row. The unique index
// guarantees there is at most one such row, so LIMIT 1 is defensive only.
const tlsAskLookupSQL = `
SELECT verified_at, tls_paused_at
FROM tenant_custom_domains
WHERE lower(host) = lower($1) AND deleted_at IS NULL
LIMIT 1
`

// Lookup implements tls_ask.Repository. Returns ErrNotFound when no active
// row exists for the host; any other failure surfaces as the wrapped error
// (use-case maps to DecisionError).
func (s *TLSAskLookup) Lookup(ctx context.Context, host string) (tls_ask.DomainRecord, error) {
	if host == "" {
		// Defensive — the use-case already rejects empty hosts at the
		// boundary; this prevents accidental "match all" if the validator
		// ever changes.
		return tls_ask.DomainRecord{}, tls_ask.ErrNotFound
	}
	var (
		verified *time.Time
		paused   *time.Time
	)
	row := s.db.QueryRow(ctx, tlsAskLookupSQL, host)
	if err := row.Scan(&verified, &paused); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tls_ask.DomainRecord{}, tls_ask.ErrNotFound
		}
		return tls_ask.DomainRecord{}, fmt.Errorf("custom_domain_lookup: %w", err)
	}
	return tls_ask.DomainRecord{
		VerifiedAt:  verified,
		TLSPausedAt: paused,
	}, nil
}

// Compile-time guard.
var _ tls_ask.Repository = (*TLSAskLookup)(nil)
