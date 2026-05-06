package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

// These unit tests pin the SQL contract of TLSAskLookup against the
// schema declared in 0010_tenant_custom_domains.up.sql. End-to-end
// behaviour against a real Postgres lives in
// internal/customdomain/integration (build-tag `integration`); both
// layers are required because the unit tests guard scan-shape
// regressions cheaply on every CI run while the integration tests
// exercise actual SQL semantics (LOWER, partial unique index, NULL
// pointer scans).

func TestTLSAskLookup_HappyPathReturnsTimestamps(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{verified, (*time.Time)(nil)}}
		},
	}
	rec, err := pgstore.NewTLSAskLookup(conn).Lookup(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if rec.VerifiedAt == nil || !rec.VerifiedAt.Equal(verified) {
		t.Fatalf("VerifiedAt = %v, want %v", rec.VerifiedAt, verified)
	}
	if rec.TLSPausedAt != nil {
		t.Fatalf("TLSPausedAt = %v, want nil", rec.TLSPausedAt)
	}
}

func TestTLSAskLookup_NoRowsReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := pgstore.NewTLSAskLookup(conn).Lookup(context.Background(), "nobody.example.com")
	if !errors.Is(err, tls_ask.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTLSAskLookup_TransientErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("conn lost")
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: boom}
		},
	}
	_, err := pgstore.NewTLSAskLookup(conn).Lookup(context.Background(), "shop.example.com")
	if err == nil || errors.Is(err, tls_ask.ErrNotFound) {
		t.Fatalf("err = %v, want generic transient error", err)
	}
}

func TestTLSAskLookup_EmptyHostShortCircuits(t *testing.T) {
	t.Parallel()
	called := false
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			called = true
			return fakeRow{}
		},
	}
	_, err := pgstore.NewTLSAskLookup(conn).Lookup(context.Background(), "")
	if !errors.Is(err, tls_ask.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if called {
		t.Fatal("QueryRow was invoked despite empty host")
	}
}
