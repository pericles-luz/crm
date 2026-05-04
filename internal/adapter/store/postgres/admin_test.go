package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/webhook"
)

// TestTokenAdmin_Insert_Happy verifies the canonical "INSERT one row"
// path returns nil. The exec stub does not assert SQL because the
// statement text is allowed to evolve as long as the surface contract
// holds; the integration job in CI exercises real Postgres.
func TestTokenAdmin_Insert_Happy(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		},
	}
	admin := pgstore.NewTokenAdmin(conn)
	if err := admin.Insert(context.Background(), webhook.TenantID{0xaa}, "whatsapp", []byte{0x01}, 5, time.Now()); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

// TestTokenAdmin_Insert_UniqueViolationTyped maps SQLSTATE 23505 onto
// the typed admin error so the CLI gives a useful operator message.
func TestTokenAdmin_Insert_UniqueViolationTyped(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value"}
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, pgErr },
	}
	admin := pgstore.NewTokenAdmin(conn)
	err := admin.Insert(context.Background(), webhook.TenantID{0xaa}, "whatsapp", []byte{0x01}, 5, time.Now())
	if !errors.Is(err, webhook.ErrTokenAlreadyActive) {
		t.Fatalf("err = %v, want ErrTokenAlreadyActive", err)
	}
}

// TestTokenAdmin_Insert_GenericError surfaces non-23505 errors
// verbatim so operators see the underlying driver message.
func TestTokenAdmin_Insert_GenericError(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errors.New("connection lost") },
	}
	admin := pgstore.NewTokenAdmin(conn)
	err := admin.Insert(context.Background(), webhook.TenantID{0xaa}, "whatsapp", []byte{0x01}, 5, time.Now())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, webhook.ErrTokenAlreadyActive) {
		t.Fatalf("generic driver error must NOT match ErrTokenAlreadyActive: %v", err)
	}
}

// TestTokenAdmin_ScheduleRevocation_Happy verifies the rotation flow:
// the UPDATE affects one row, no error.
func TestTokenAdmin_ScheduleRevocation_Happy(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	admin := pgstore.NewTokenAdmin(conn)
	if err := admin.ScheduleRevocation(context.Background(), "whatsapp", []byte{0x01}, time.Now()); err != nil {
		t.Fatalf("ScheduleRevocation: %v", err)
	}
}

// TestTokenAdmin_ScheduleRevocation_NotFound maps "0 rows affected"
// onto the typed admin error so the CLI can tell operators they
// supplied the wrong --rotate-from-token-hash-hex.
func TestTokenAdmin_ScheduleRevocation_NotFound(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
	}
	admin := pgstore.NewTokenAdmin(conn)
	err := admin.ScheduleRevocation(context.Background(), "whatsapp", []byte{0x01}, time.Now())
	if !errors.Is(err, webhook.ErrTokenNotFound) {
		t.Fatalf("err = %v, want ErrTokenNotFound", err)
	}
}

// TestTokenAdmin_ScheduleRevocation_GenericError surfaces driver
// errors without the typed admin sentinel.
func TestTokenAdmin_ScheduleRevocation_GenericError(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errors.New("driver dead") },
	}
	admin := pgstore.NewTokenAdmin(conn)
	err := admin.ScheduleRevocation(context.Background(), "whatsapp", []byte{0x01}, time.Now())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, webhook.ErrTokenNotFound) {
		t.Fatalf("driver error must NOT match ErrTokenNotFound: %v", err)
	}
}
