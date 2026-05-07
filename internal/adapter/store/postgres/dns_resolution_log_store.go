package postgres

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/validation"
)

// DNSResolutionLogStore is the production implementation of
// validation.Writer. It persists one row per Validate / ValidateHostOnly
// terminal outcome (allow / block / error) into dns_resolution_log
// (migration 0010 + 0011).
//
// SIN-62333 / OWASP A09: pre-F52 the validator emitted EventBlockedSSRF
// to slog only — IR engineers had no queryable timeline of which tenant
// attempted which hostname → which IP. This adapter closes that gap by
// landing every decision, including blocks, into a queryable table
// keyed on tenant_id + host + decision + reason.
//
// Anti-IP-leak: blocked-SSRF rows MUST NOT carry an IP. The validator
// already builds a LogEntry with PinnedIP == zero on block paths, but
// we also defend in depth by writing NULL to pinned_ip whenever the
// decision is "block" or PinnedIP is invalid. The CHECK constraint
// `dns_resolution_log_block_no_ip_chk` rejects any row that violates
// the invariant at the database boundary.
type DNSResolutionLogStore struct {
	db PgxConn
}

// NewDNSResolutionLogStore returns a store bound to the given pgx pool
// or transaction-shaped surface.
func NewDNSResolutionLogStore(db PgxConn) *DNSResolutionLogStore {
	return &DNSResolutionLogStore{db: db}
}

// dnsResolutionLogInsertSQL writes one row into dns_resolution_log.
// Pinned IP is bound as `host(inet)`-shape `text`; we keep the bind
// simple by pre-encoding to string at the boundary so callers do not
// need a pgx-specific INET wrapper.
const dnsResolutionLogInsertSQL = `
INSERT INTO dns_resolution_log
    (id, tenant_id, host, pinned_ip, verified_with_dnssec,
     decision, reason, phase, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`

// Write persists one entry. Returns nil on success, a wrapped error on
// failure. The validation package's Validator.emit swallows that error
// (Writer outage must not deny legitimate validations) — we still
// surface it so unit-test doubles can assert the call shape.
func (s *DNSResolutionLogStore) Write(ctx context.Context, e validation.LogEntry) error {
	id := uuid.New()
	tenantArg := tenantBindArg(e.TenantID)
	ipArg := pinnedIPBindArg(e.PinnedIP, e.Decision)
	if _, err := s.db.Exec(ctx, dnsResolutionLogInsertSQL,
		id,
		tenantArg,
		e.Host,
		ipArg,
		e.VerifiedWithDNSSEC,
		e.Decision,
		e.Reason,
		e.Phase,
		e.At,
	); err != nil {
		return fmt.Errorf("dns_resolution_log insert: %w", err)
	}
	return nil
}

// tenantBindArg returns nil for uuid.Nil so the row stores SQL NULL —
// forensics queries can then distinguish anonymous attempts from
// tenant-scoped ones with `WHERE tenant_id IS NULL`.
func tenantBindArg(tenantID uuid.UUID) any {
	if tenantID == uuid.Nil {
		return nil
	}
	return tenantID
}

// pinnedIPBindArg enforces the anti-leak invariant: any block decision
// erases the IP, even if the validator passed a non-zero address. The
// validator already does this on its side, but defence in depth at the
// adapter boundary closes the door if a future caller forgets.
func pinnedIPBindArg(addr netip.Addr, decision string) any {
	if !addr.IsValid() || decision == validation.DecisionBlock {
		return nil
	}
	return addr.String()
}

// Compile-time guard.
var _ validation.Writer = (*DNSResolutionLogStore)(nil)
