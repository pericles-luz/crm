package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

type instagramTokenRow struct {
	accessToken string
	expiresAt   time.Time
	err         error
}

func (r instagramTokenRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	tokPtr, ok := dest[0].(*string)
	if !ok {
		return errors.New("scan: dest[0] not *string")
	}
	expPtr, ok := dest[1].(*time.Time)
	if !ok {
		return errors.New("scan: dest[1] not *time.Time")
	}
	*tokPtr = r.accessToken
	*expPtr = r.expiresAt
	return nil
}

type instagramTokenConn struct {
	queryRow func(sql string, args ...any) pgx.Row
	exec     func(sql string, args ...any) (pgconn.CommandTag, error)
}

func (c instagramTokenConn) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return c.queryRow(sql, args...)
}

func (c instagramTokenConn) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return c.exec(sql, args...)
}

func TestInstagramOAuthTokenStore_Save(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	expiresAt := time.Now().Add(60 * 24 * time.Hour)
	var gotSQL string
	var gotArgs []any
	conn := instagramTokenConn{
		exec: func(sql string, args ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			gotArgs = args
			return pgconn.CommandTag{}, nil
		},
	}
	err := pgstore.NewInstagramOAuthTokenStore(conn).Save(context.Background(), tenantID, "tok-abc", "bearer", expiresAt)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.Contains(gotSQL, "INSERT INTO instagram_oauth_tokens") {
		t.Errorf("SQL missing INSERT target:\n%s", gotSQL)
	}
	if !strings.Contains(gotSQL, "ON CONFLICT (tenant_id) DO UPDATE") {
		t.Errorf("SQL missing upsert clause:\n%s", gotSQL)
	}
	if len(gotArgs) != 4 {
		t.Fatalf("want 4 args, got %d: %v", len(gotArgs), gotArgs)
	}
	if gotArgs[0] != tenantID || gotArgs[1] != "tok-abc" || gotArgs[2] != "bearer" || gotArgs[3] != expiresAt {
		t.Errorf("unexpected args: %v", gotArgs)
	}
}

func TestInstagramOAuthTokenStore_Save_ErrorWrapped(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk full")
	conn := instagramTokenConn{
		exec: func(string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, boom
		},
	}
	err := pgstore.NewInstagramOAuthTokenStore(conn).Save(context.Background(), uuid.New(), "tok", "bearer", time.Now())
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped original error, got %v", err)
	}
}

func TestInstagramOAuthTokenStore_Get_Found(t *testing.T) {
	t.Parallel()
	expiresAt := time.Now().Add(60 * 24 * time.Hour)
	conn := instagramTokenConn{
		queryRow: func(string, ...any) pgx.Row {
			return instagramTokenRow{accessToken: "tok-abc", expiresAt: expiresAt}
		},
	}
	tok, exp, ok, err := pgstore.NewInstagramOAuthTokenStore(conn).Get(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if tok != "tok-abc" {
		t.Errorf("token = %q", tok)
	}
	if !exp.Equal(expiresAt) {
		t.Errorf("expiresAt = %v, want %v", exp, expiresAt)
	}
}

func TestInstagramOAuthTokenStore_Get_NotFound(t *testing.T) {
	t.Parallel()
	conn := instagramTokenConn{
		queryRow: func(string, ...any) pgx.Row {
			return instagramTokenRow{err: pgx.ErrNoRows}
		},
	}
	tok, _, ok, err := pgstore.NewInstagramOAuthTokenStore(conn).Get(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("expected no error for not-found, got %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
}

func TestInstagramOAuthTokenStore_Get_ErrorWrapped(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection reset")
	conn := instagramTokenConn{
		queryRow: func(string, ...any) pgx.Row {
			return instagramTokenRow{err: boom}
		},
	}
	_, _, _, err := pgstore.NewInstagramOAuthTokenStore(conn).Get(context.Background(), uuid.New())
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped original error, got %v", err)
	}
}
