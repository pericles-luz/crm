package postgres_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/webhook"
)

// fakeRow scriptes the QueryRow result. Real Postgres integration tests
// live behind testcontainers and run separately (//go:build integration).
type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
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
			} else if v, ok := r.values[i].([]byte); ok && len(v) == 16 {
				copy(p[:], v)
			} else {
				return errors.New("scan: cannot map to [16]byte")
			}
		case **time.Time:
			switch v := r.values[i].(type) {
			case nil:
				*p = nil
			case *time.Time:
				*p = v
			case time.Time:
				cp := v
				*p = &cp
			default:
				return errors.New("scan: bad *time.Time")
			}
		case *int:
			if v, ok := r.values[i].(int); ok {
				*p = v
			} else {
				return errors.New("scan: bad int")
			}
		case *bool:
			if v, ok := r.values[i].(bool); ok {
				*p = v
			} else {
				return errors.New("scan: bad bool")
			}
		case *[]byte:
			if v, ok := r.values[i].([]byte); ok {
				*p = v
			} else {
				return errors.New("scan: bad []byte")
			}
		default:
			_ = driver.Valuer(nil)
			return errors.New("scan: unsupported dest")
		}
	}
	return nil
}

type stubConn struct {
	queryRow func(sql string, args ...any) pgx.Row
	exec     func(sql string, args ...any) (pgconn.CommandTag, error)
}

func (s stubConn) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return s.queryRow(sql, args...)
}
func (s stubConn) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return s.exec(sql, args...)
}

func TestTokenStore_LookupHappyPath(t *testing.T) {
	t.Parallel()
	tenant := [16]byte{0xaa}
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{tenant, (*time.Time)(nil)}}
		},
	}
	got, err := pgstore.NewTokenStore(conn).Lookup(context.Background(), "whatsapp", []byte{0x01}, time.Now())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if webhook.TenantID(tenant) != got {
		t.Fatalf("tenant = %v, want %v", got, tenant)
	}
}

// rev 3 / F-13: revoked_at is the scheduled effective time, so a
// revoked_at in the past means "already revoked, lookup fails".
func TestTokenStore_LookupRevokedInPast(t *testing.T) {
	t.Parallel()
	tenant := [16]byte{0xaa}
	revoked := time.Now().Add(-time.Hour) // already past — token is revoked
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{tenant, revoked}}
		},
	}
	_, err := pgstore.NewTokenStore(conn).Lookup(context.Background(), "whatsapp", []byte{0x01}, time.Now())
	if !errors.Is(err, webhook.ErrTokenRevoked) {
		t.Fatalf("err = %v, want ErrTokenRevoked", err)
	}
}

// rev 3 / F-13: token whose revoked_at is in the future is still valid
// (rotation set a grace window). T-G6 maps to this case.
func TestTokenStore_LookupRevokedInFutureStillValid(t *testing.T) {
	t.Parallel()
	tenant := [16]byte{0xaa}
	now := time.Now()
	revoked := now.Add(3 * time.Minute) // grace window still open
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{tenant, revoked}}
		},
	}
	got, err := pgstore.NewTokenStore(conn).Lookup(context.Background(), "whatsapp", []byte{0x01}, now)
	if err != nil {
		t.Fatalf("Lookup within grace: %v", err)
	}
	if got != webhook.TenantID(tenant) {
		t.Fatalf("tenant mismatch")
	}
}

func TestTokenStore_LookupUnknown(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := pgstore.NewTokenStore(conn).Lookup(context.Background(), "whatsapp", []byte{0x01}, time.Now())
	if !errors.Is(err, webhook.ErrTokenUnknown) {
		t.Fatalf("err = %v, want ErrTokenUnknown", err)
	}
}

func TestTokenStore_LookupOtherError(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: errors.New("driver lost")}
		},
	}
	_, err := pgstore.NewTokenStore(conn).Lookup(context.Background(), "whatsapp", []byte{0x01}, time.Now())
	if err == nil || errors.Is(err, webhook.ErrTokenUnknown) || errors.Is(err, webhook.ErrTokenRevoked) {
		t.Fatalf("err = %v, want generic error", err)
	}
}

func TestTokenStore_MarkUsed(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil },
	}
	if err := pgstore.NewTokenStore(conn).MarkUsed(context.Background(), "whatsapp", []byte{0x01}, time.Now()); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	connBad := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errors.New("conn lost") },
	}
	if err := pgstore.NewTokenStore(connBad).MarkUsed(context.Background(), "whatsapp", []byte{0x01}, time.Now()); err == nil {
		t.Fatal("expected error")
	}
}

func TestIdempotencyStore_FirstSeenAndConflict(t *testing.T) {
	t.Parallel()
	first := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{[]byte{0x01, 0x02}}}
		},
	}
	store := pgstore.NewIdempotencyStore(first)
	ok, err := store.CheckAndStore(context.Background(), webhook.TenantID{0xaa}, "whatsapp", []byte{0x01, 0x02}, time.Now())
	if err != nil || !ok {
		t.Fatalf("first insert: ok=%v err=%v", ok, err)
	}

	second := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	store2 := pgstore.NewIdempotencyStore(second)
	ok, err = store2.CheckAndStore(context.Background(), webhook.TenantID{0xaa}, "whatsapp", []byte{0x01, 0x02}, time.Now())
	if err != nil || ok {
		t.Fatalf("conflict insert: ok=%v err=%v", ok, err)
	}
}

func TestIdempotencyStore_ScanError(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: errors.New("driver lost")}
		},
	}
	_, err := pgstore.NewIdempotencyStore(conn).CheckAndStore(context.Background(), webhook.TenantID{}, "whatsapp", []byte{}, time.Now())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRawEventStore_InsertAndMark(t *testing.T) {
	t.Parallel()
	id := [16]byte{1, 2, 3}
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{id}}
		},
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil },
	}
	store := pgstore.NewRawEventStore(conn)
	got, err := store.Insert(context.Background(), webhook.RawEventRow{
		TenantID:       webhook.TenantID{0xaa},
		Channel:        "whatsapp",
		IdempotencyKey: []byte{0x01},
		Payload:        []byte("hello"),
		Headers:        map[string][]string{"X-Test": {"v"}},
		ReceivedAt:     time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got != id {
		t.Fatalf("id mismatch")
	}
	if err := store.MarkPublished(context.Background(), got, time.Now()); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
}

func TestRawEventStore_InsertError(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row { return fakeRow{err: errors.New("disk full")} },
	}
	_, err := pgstore.NewRawEventStore(conn).Insert(context.Background(), webhook.RawEventRow{Channel: "whatsapp"})
	if err == nil {
		t.Fatal("expected error")
	}
}

// rev 3 / F-12: TenantAssociationStore returns true iff the
// (tenant, channel, association) tuple exists in
// tenant_channel_associations.
func TestTenantAssociationStore_CheckMatch(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{true}}
		},
	}
	ok, err := pgstore.NewTenantAssociationStore(conn).CheckAssociation(context.Background(), webhook.TenantID{0xaa}, "whatsapp", "phone_for_A")
	if err != nil {
		t.Fatalf("CheckAssociation: %v", err)
	}
	if !ok {
		t.Fatal("expected match")
	}
}

func TestTenantAssociationStore_CheckMismatch(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{values: []any{false}}
		},
	}
	ok, err := pgstore.NewTenantAssociationStore(conn).CheckAssociation(context.Background(), webhook.TenantID{0xaa}, "whatsapp", "stranger")
	if err != nil {
		t.Fatalf("CheckAssociation: %v", err)
	}
	if ok {
		t.Fatal("expected mismatch")
	}
}

func TestTenantAssociationStore_DBError(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: errors.New("connection lost")}
		},
	}
	_, err := pgstore.NewTenantAssociationStore(conn).CheckAssociation(context.Background(), webhook.TenantID{}, "x", "y")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestTenantAssociationStore_NoRowsReturnsFalse(t *testing.T) {
	t.Parallel()
	// EXISTS always returns one row in real PG; we still cover the
	// defensive ErrNoRows branch.
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	ok, err := pgstore.NewTenantAssociationStore(conn).CheckAssociation(context.Background(), webhook.TenantID{}, "x", "y")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want false,nil", ok, err)
	}
}

func TestRawEventStore_MarkPublishedError(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errors.New("disk full") },
	}
	if err := pgstore.NewRawEventStore(conn).MarkPublished(context.Background(), [16]byte{}, time.Now()); err == nil {
		t.Fatal("expected error")
	}
}
