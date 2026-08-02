package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

type igBusinessIDRow struct {
	value string
	err   error
}

func (r igBusinessIDRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("scan: dest[0] not *string")
	}
	*p = r.value
	return nil
}

type igBusinessIDConn struct {
	queryRow func(sql string, args ...any) pgx.Row
}

func (c igBusinessIDConn) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return c.queryRow(sql, args...)
}

func (c igBusinessIDConn) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected Exec call")
}

func TestInstagramIGBusinessIDLookup_Found(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	var gotSQL string
	var gotArgs []any
	conn := igBusinessIDConn{
		queryRow: func(sql string, args ...any) pgx.Row {
			gotSQL = sql
			gotArgs = args
			return igBusinessIDRow{value: "ig123"}
		},
	}
	got, err := pgstore.NewInstagramIGBusinessIDLookup(conn).IGBusinessID(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("IGBusinessID: %v", err)
	}
	if got != "ig123" {
		t.Errorf("got %q, want ig123", got)
	}
	if !strings.Contains(gotSQL, "FROM tenant_channel_associations") {
		t.Errorf("SQL missing target table:\n%s", gotSQL)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "instagram" || gotArgs[1] != tenantID {
		t.Errorf("unexpected args: %v", gotArgs)
	}
}

func TestInstagramIGBusinessIDLookup_NotFound(t *testing.T) {
	t.Parallel()
	conn := igBusinessIDConn{
		queryRow: func(string, ...any) pgx.Row {
			return igBusinessIDRow{err: pgx.ErrNoRows}
		},
	}
	got, err := pgstore.NewInstagramIGBusinessIDLookup(conn).IGBusinessID(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("expected no error for not-found, got %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestInstagramIGBusinessIDLookup_ErrorWrapped(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection reset")
	conn := igBusinessIDConn{
		queryRow: func(string, ...any) pgx.Row {
			return igBusinessIDRow{err: boom}
		},
	}
	_, err := pgstore.NewInstagramIGBusinessIDLookup(conn).IGBusinessID(context.Background(), uuid.New())
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped original error, got %v", err)
	}
}
