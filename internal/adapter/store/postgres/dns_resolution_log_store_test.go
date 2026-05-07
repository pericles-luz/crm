package postgres_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/customdomain/validation"
)

// recordingConn captures the Exec arguments so tests can assert the
// SQL bindings without needing a real Postgres. Integration tests
// (build tag `integration`) cover the actual write path against a
// migrated database.
type recordingConn struct {
	gotSQL  string
	gotArgs []any
	execErr error
}

func (c *recordingConn) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	panic("dns_resolution_log Write must not call QueryRow")
}

func (c *recordingConn) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.gotSQL = sql
	c.gotArgs = args
	return pgconn.CommandTag{}, c.execErr
}

func TestDNSResolutionLogStore_HappyPath_BindsAllColumns(t *testing.T) {
	t.Parallel()
	conn := &recordingConn{}
	store := pgstore.NewDNSResolutionLogStore(conn)

	tenantID := uuid.New()
	at := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	entry := validation.LogEntry{
		TenantID:           tenantID,
		Host:               "shop.example.com",
		PinnedIP:           netip.MustParseAddr("203.0.113.10"),
		VerifiedWithDNSSEC: true,
		Decision:           validation.DecisionAllow,
		Reason:             validation.ReasonOK,
		Phase:              validation.PhaseValidate,
		At:                 at,
	}
	if err := store.Write(context.Background(), entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conn.gotArgs) != 9 {
		t.Fatalf("expected 9 args, got %d", len(conn.gotArgs))
	}
	if _, ok := conn.gotArgs[0].(uuid.UUID); !ok {
		t.Fatalf("arg[0] must be the generated row id (uuid.UUID), got %T", conn.gotArgs[0])
	}
	if got, _ := conn.gotArgs[1].(uuid.UUID); got != tenantID {
		t.Fatalf("arg[1] tenant_id = %v, want %s", conn.gotArgs[1], tenantID)
	}
	if conn.gotArgs[2] != "shop.example.com" {
		t.Fatalf("arg[2] host = %v", conn.gotArgs[2])
	}
	if conn.gotArgs[3] != "203.0.113.10" {
		t.Fatalf("arg[3] pinned_ip = %v, want 203.0.113.10", conn.gotArgs[3])
	}
	if conn.gotArgs[4] != true {
		t.Fatalf("arg[4] dnssec = %v", conn.gotArgs[4])
	}
	if conn.gotArgs[5] != "allow" || conn.gotArgs[6] != "ok" || conn.gotArgs[7] != "validate" {
		t.Fatalf("decision/reason/phase = %v / %v / %v", conn.gotArgs[5], conn.gotArgs[6], conn.gotArgs[7])
	}
	if conn.gotArgs[8] != at {
		t.Fatalf("arg[8] at = %v, want %v", conn.gotArgs[8], at)
	}
}

func TestDNSResolutionLogStore_AnonymousCall_BindsTenantIDAsNil(t *testing.T) {
	// Forensics requires that anonymous validations land tenant_id NULL,
	// not uuid.Nil. The adapter binds Go nil so pgx writes SQL NULL.
	t.Parallel()
	conn := &recordingConn{}
	store := pgstore.NewDNSResolutionLogStore(conn)

	if err := store.Write(context.Background(), validation.LogEntry{
		Host:     "anon.example",
		Decision: validation.DecisionAllow,
		Reason:   validation.ReasonOK,
		Phase:    validation.PhaseValidate,
		At:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn.gotArgs[1] != nil {
		t.Fatalf("tenant_id arg = %v, want nil", conn.gotArgs[1])
	}
}

func TestDNSResolutionLogStore_BlockDecision_NeverBindsResolvedIP(t *testing.T) {
	// Defence in depth: even if a future caller forgets to scrub the
	// IP from the LogEntry, the adapter must still bind nil for any
	// row marked block.
	t.Parallel()
	conn := &recordingConn{}
	store := pgstore.NewDNSResolutionLogStore(conn)

	leakyEntry := validation.LogEntry{
		Host:     "evil.example",
		PinnedIP: netip.MustParseAddr("127.0.0.1"),
		Decision: validation.DecisionBlock,
		Reason:   validation.ReasonPrivateIP,
		Phase:    validation.PhaseValidate,
		At:       time.Now().UTC(),
	}
	if err := store.Write(context.Background(), leakyEntry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn.gotArgs[3] != nil {
		t.Fatalf("pinned_ip leaked on block row: %v", conn.gotArgs[3])
	}
}

func TestDNSResolutionLogStore_InvalidIPAddr_BindsNil(t *testing.T) {
	t.Parallel()
	conn := &recordingConn{}
	store := pgstore.NewDNSResolutionLogStore(conn)

	if err := store.Write(context.Background(), validation.LogEntry{
		Host:     "noaddr.example",
		PinnedIP: netip.Addr{},
		Decision: validation.DecisionError,
		Reason:   validation.ReasonNoAddress,
		Phase:    validation.PhaseValidate,
		At:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn.gotArgs[3] != nil {
		t.Fatalf("invalid pinned_ip should bind nil, got %v", conn.gotArgs[3])
	}
}

func TestDNSResolutionLogStore_PropagatesExecError(t *testing.T) {
	t.Parallel()
	boom := errors.New("postgres down")
	conn := &recordingConn{execErr: boom}
	store := pgstore.NewDNSResolutionLogStore(conn)

	err := store.Write(context.Background(), validation.LogEntry{
		Host:     "x.example",
		Decision: validation.DecisionAllow,
		Reason:   validation.ReasonOK,
		Phase:    validation.PhaseValidate,
		At:       time.Now().UTC(),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrap of %v", err, boom)
	}
}
