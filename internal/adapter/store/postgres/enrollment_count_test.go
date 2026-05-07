package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

// int64Row scriptes the COUNT(*)::bigint scan into *int64 / pgx.ErrNoRows
// without modifying the shared fakeRow. Same pattern as store_test.go.
type int64Row struct {
	value int64
	err   error
}

func (r int64Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("scan: arity mismatch")
	}
	switch p := dest[0].(type) {
	case *int64:
		*p = r.value
		return nil
	default:
		return errors.New("scan: unsupported dest")
	}
}

func TestEnrollmentCountStore_ActiveCount_HappyPath(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conn := stubConn{
		queryRow: func(_ string, args ...any) pgx.Row {
			if len(args) != 1 {
				t.Fatalf("expected 1 arg, got %d", len(args))
			}
			if args[0] != tenant {
				t.Fatalf("expected tenant arg %v, got %v", tenant, args[0])
			}
			return int64Row{value: 7}
		},
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil },
	}
	got, err := pgstore.NewEnrollmentCountStore(conn).ActiveCount(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	if got != 7 {
		t.Fatalf("count = %d, want 7", got)
	}
}

func TestEnrollmentCountStore_ActiveCount_ZeroOnNoRows(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return int64Row{err: pgx.ErrNoRows}
		},
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil },
	}
	got, err := pgstore.NewEnrollmentCountStore(conn).ActiveCount(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	if got != 0 {
		t.Fatalf("count = %d, want 0 on ErrNoRows", got)
	}
}

func TestEnrollmentCountStore_ActiveCount_PropagatesScanError(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	want := errors.New("driver lost")
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return int64Row{err: want}
		},
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil },
	}
	_, err := pgstore.NewEnrollmentCountStore(conn).ActiveCount(context.Background(), tenant)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrapped %v", err, want)
	}
}

func TestEnrollmentCountStore_ActiveCount_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row { t.Fatal("queryRow should not be called for uuid.Nil"); return nil },
		exec:     func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil },
	}
	_, err := pgstore.NewEnrollmentCountStore(conn).ActiveCount(context.Background(), uuid.Nil)
	if err == nil {
		t.Fatal("expected error for uuid.Nil tenant")
	}
}

func TestEnrollmentCountStore_ActiveCount_NegativeIsClampedToZero(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return int64Row{value: -5}
		},
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil },
	}
	got, err := pgstore.NewEnrollmentCountStore(conn).ActiveCount(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	if got != 0 {
		t.Fatalf("negative result not clamped: got %d, want 0", got)
	}
}
