package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

// slugRow is a richer row stub than fakeRow because the slug-reservation
// schema uses *string and *time.Time directly.
type slugRow struct {
	values []any
	err    error
}

func (r slugRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("scan: arity mismatch")
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *[16]byte:
			if v, ok := r.values[i].([16]byte); ok {
				*p = v
			} else {
				return errors.New("scan: bad [16]byte")
			}
		case **[16]byte:
			if r.values[i] == nil {
				*p = nil
				continue
			}
			if v, ok := r.values[i].([16]byte); ok {
				cp := v
				*p = &cp
			} else {
				return errors.New("scan: bad **[16]byte")
			}
		case *string:
			if v, ok := r.values[i].(string); ok {
				*p = v
			} else {
				return errors.New("scan: bad *string")
			}
		case *time.Time:
			if v, ok := r.values[i].(time.Time); ok {
				*p = v
			} else {
				return errors.New("scan: bad *time.Time")
			}
		default:
			return errors.New("scan: unsupported dest")
		}
	}
	return nil
}

type slugStub struct {
	queryRow func(sql string, args ...any) pgx.Row
	exec     func(sql string, args ...any) (pgconn.CommandTag, error)
}

func (s slugStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return s.queryRow(sql, args...)
}
func (s slugStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if s.exec == nil {
		return pgconn.CommandTag{}, nil
	}
	return s.exec(sql, args...)
}

func reservationRow(id uuid.UUID, slug string, releasedAt, expiresAt, createdAt time.Time, releasedBy *uuid.UUID) slugRow {
	var by any
	if releasedBy != nil {
		by = [16]byte(*releasedBy)
	}
	return slugRow{values: []any{[16]byte(id), slug, releasedAt, by, expiresAt, createdAt}}
}

func TestSlugReservationStore_Active_HappyPath(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	tenantID := uuid.New()
	now := time.Now().UTC()
	exp := now.Add(slugreservation.ReservationWindow)
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return reservationRow(id, "acme", now, exp, now, &tenantID)
	}}
	got, err := pgstore.NewSlugReservationStore(conn).Active(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got.ID != id || got.Slug != "acme" {
		t.Fatalf("got=%+v", got)
	}
	if got.ReleasedByTenantID != tenantID {
		t.Fatalf("releasedBy=%v", got.ReleasedByTenantID)
	}
	if !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expires=%v want=%v", got.ExpiresAt, exp)
	}
}

func TestSlugReservationStore_Active_NotFound(t *testing.T) {
	t.Parallel()
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return slugRow{err: pgx.ErrNoRows}
	}}
	if _, err := pgstore.NewSlugReservationStore(conn).Active(context.Background(), "acme"); !errors.Is(err, slugreservation.ErrNotReserved) {
		t.Fatalf("err=%v", err)
	}
}

func TestSlugReservationStore_Active_OtherError(t *testing.T) {
	t.Parallel()
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return slugRow{err: errors.New("conn lost")}
	}}
	if _, err := pgstore.NewSlugReservationStore(conn).Active(context.Background(), "acme"); err == nil || errors.Is(err, slugreservation.ErrNotReserved) {
		t.Fatalf("err=%v", err)
	}
}

func TestSlugReservationStore_Insert_HappyPath(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	tenant := uuid.New()
	now := time.Now().UTC()
	exp := now.Add(slugreservation.ReservationWindow)
	conn := slugStub{queryRow: func(_ string, args ...any) pgx.Row {
		if len(args) != 4 || args[0] != "acme" {
			return slugRow{err: errors.New("bad args")}
		}
		return reservationRow(id, "acme", now, exp, now, &tenant)
	}}
	got, err := pgstore.NewSlugReservationStore(conn).Insert(context.Background(), "acme", tenant, now, exp)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got.ID != id || got.Slug != "acme" {
		t.Fatalf("got=%+v", got)
	}
}

func TestSlugReservationStore_Insert_NilTenantOK(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	conn := slugStub{queryRow: func(_ string, args ...any) pgx.Row {
		if args[2] != nil {
			return slugRow{err: errors.New("expected nil tenant arg")}
		}
		return reservationRow(id, "acme", now, exp, now, nil)
	}}
	got, err := pgstore.NewSlugReservationStore(conn).Insert(context.Background(), "acme", uuid.Nil, now, exp)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got.ReleasedByTenantID != uuid.Nil {
		t.Fatal("expected nil tenant")
	}
}

func TestSlugReservationStore_Insert_UniqueViolation(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	tenant := uuid.New()
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	pgErr := &pgconn.PgError{Code: "23505", Message: "unique_violation"}

	calls := 0
	conn := slugStub{queryRow: func(sql string, _ ...any) pgx.Row {
		calls++
		if calls == 1 {
			// INSERT — return the unique-violation
			return slugRow{err: pgErr}
		}
		// SELECT active — return the existing row
		return reservationRow(id, "acme", now, exp, now, &tenant)
	}}
	_, err := pgstore.NewSlugReservationStore(conn).Insert(context.Background(), "acme", tenant, now, exp)
	if err == nil {
		t.Fatal("expected error")
	}
	var rerr *slugreservation.ReservedError
	if !errors.As(err, &rerr) {
		t.Fatalf("err=%v want *ReservedError", err)
	}
	if rerr.Reservation.Slug != "acme" {
		t.Fatalf("res=%+v", rerr.Reservation)
	}
}

func TestSlugReservationStore_SoftDelete_HappyPath(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	tenant := uuid.New()
	now := time.Now().UTC()
	conn := slugStub{queryRow: func(_ string, _ ...any) pgx.Row {
		return reservationRow(id, "acme", now.Add(-time.Hour), now, now.Add(-time.Hour), &tenant)
	}}
	got, err := pgstore.NewSlugReservationStore(conn).SoftDelete(context.Background(), "acme", now)
	if err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if !got.ExpiresAt.Equal(now) {
		t.Fatalf("expires=%v", got.ExpiresAt)
	}
}

func TestSlugReservationStore_SoftDelete_NotFound(t *testing.T) {
	t.Parallel()
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return slugRow{err: pgx.ErrNoRows}
	}}
	if _, err := pgstore.NewSlugReservationStore(conn).SoftDelete(context.Background(), "acme", time.Now()); !errors.Is(err, slugreservation.ErrNotReserved) {
		t.Fatalf("err=%v", err)
	}
}

func TestSlugRedirectStore_Active_HappyPath(t *testing.T) {
	t.Parallel()
	exp := time.Now().Add(time.Hour).UTC()
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return slugRow{values: []any{"acme", "acme-2", exp}}
	}}
	got, err := pgstore.NewSlugRedirectStore(conn).Active(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got.OldSlug != "acme" || got.NewSlug != "acme-2" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("got=%+v", got)
	}
}

func TestSlugRedirectStore_Active_NotFound(t *testing.T) {
	t.Parallel()
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return slugRow{err: pgx.ErrNoRows}
	}}
	if _, err := pgstore.NewSlugRedirectStore(conn).Active(context.Background(), "acme"); !errors.Is(err, slugreservation.ErrNotReserved) {
		t.Fatalf("err=%v", err)
	}
}

func TestSlugRedirectStore_Active_OtherError(t *testing.T) {
	t.Parallel()
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return slugRow{err: errors.New("conn lost")}
	}}
	if _, err := pgstore.NewSlugRedirectStore(conn).Active(context.Background(), "acme"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSlugRedirectStore_Upsert_HappyPath(t *testing.T) {
	t.Parallel()
	exp := time.Now().Add(time.Hour).UTC()
	conn := slugStub{queryRow: func(_ string, args ...any) pgx.Row {
		if args[0] != "acme" || args[1] != "acme-2" {
			return slugRow{err: errors.New("bad args")}
		}
		return slugRow{values: []any{"acme", "acme-2", exp}}
	}}
	got, err := pgstore.NewSlugRedirectStore(conn).Upsert(context.Background(), "acme", "acme-2", exp)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got.NewSlug != "acme-2" {
		t.Fatalf("got=%+v", got)
	}
}

func TestSlugRedirectStore_Upsert_Error(t *testing.T) {
	t.Parallel()
	conn := slugStub{queryRow: func(string, ...any) pgx.Row {
		return slugRow{err: errors.New("conflict")}
	}}
	if _, err := pgstore.NewSlugRedirectStore(conn).Upsert(context.Background(), "acme", "acme-2", time.Now()); err == nil {
		t.Fatal("expected error")
	}
}
