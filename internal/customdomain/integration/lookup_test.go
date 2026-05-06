//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

// insertCustomDomain inserts a row into tenant_custom_domains for the
// scenarios the F45 acceptance criteria call out. Pointers are written
// directly so callers can express NULL semantics for verified_at/
// tls_paused_at/deleted_at.
func insertCustomDomain(t *testing.T, h *harness, host string, verifiedAt, tlsPausedAt, deletedAt *time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const sql = `
INSERT INTO tenant_custom_domains
  (id, tenant_id, host, verification_token, verified_at, tls_paused_at, deleted_at)
VALUES
  ($1, $2, $3, $4, $5, $6, $7)
`
	if _, err := h.pool.Exec(ctx, sql,
		uuid.New(), uuid.New(), host, "tok-"+host,
		verifiedAt, tlsPausedAt, deletedAt,
	); err != nil {
		t.Fatalf("insert %q: %v", host, err)
	}
}

func TestTLSAskLookup_AllowsVerifiedActiveDomain(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)
	verified := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	insertCustomDomain(t, h, "shop.example.com", &verified, nil, nil)

	repo := pgstore.NewTLSAskLookup(h.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec, err := repo.Lookup(ctx, "shop.example.com")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if rec.VerifiedAt == nil || !rec.VerifiedAt.Equal(verified) {
		t.Fatalf("VerifiedAt = %v, want %v", rec.VerifiedAt, verified)
	}
	if rec.TLSPausedAt != nil {
		t.Fatalf("TLSPausedAt = %v, want nil", rec.TLSPausedAt)
	}
}

func TestTLSAskLookup_NeverRegistered_NotFound(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)
	repo := pgstore.NewTLSAskLookup(h.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := repo.Lookup(ctx, "nobody.example.com")
	if !errors.Is(err, tls_ask.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTLSAskLookup_VerifiedAtNull_RecordReturnedWithNilVerified(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)
	insertCustomDomain(t, h, "pending.example.com", nil, nil, nil)

	repo := pgstore.NewTLSAskLookup(h.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := repo.Lookup(ctx, "pending.example.com")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if rec.VerifiedAt != nil {
		t.Fatalf("VerifiedAt = %v, want nil", rec.VerifiedAt)
	}
}

func TestTLSAskLookup_PausedRowReturnsTLSPaused(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)
	v := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	p := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	insertCustomDomain(t, h, "frozen.example.com", &v, &p, nil)

	repo := pgstore.NewTLSAskLookup(h.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := repo.Lookup(ctx, "frozen.example.com")
	if err != nil {
		t.Fatalf("Lookup err: %v", err)
	}
	if rec.TLSPausedAt == nil || !rec.TLSPausedAt.Equal(p) {
		t.Fatalf("TLSPausedAt = %v, want %v", rec.TLSPausedAt, p)
	}
}

func TestTLSAskLookup_SoftDeletedRowsAreInvisible(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)
	v := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	d := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	insertCustomDomain(t, h, "removed.example.com", &v, nil, &d)

	repo := pgstore.NewTLSAskLookup(h.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := repo.Lookup(ctx, "removed.example.com")
	if !errors.Is(err, tls_ask.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTLSAskLookup_HostMatchIsCaseInsensitive(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)
	v := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	insertCustomDomain(t, h, "Shop.Example.COM", &v, nil, nil)

	repo := pgstore.NewTLSAskLookup(h.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := repo.Lookup(ctx, "shop.example.com")
	if err != nil {
		t.Fatalf("Lookup err: %v (case-insensitive match expected)", err)
	}
}

func TestTLSAskLookup_EmptyHostReturnsNotFoundWithoutQuery(t *testing.T) {
	h := startHarness(t)
	repo := pgstore.NewTLSAskLookup(h.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := repo.Lookup(ctx, "")
	if !errors.Is(err, tls_ask.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
