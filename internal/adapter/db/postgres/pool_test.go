package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

// Pure-Go unit branches. The successful path is exercised by
// TestNew_PingsRealPostgres below using the package's testpg harness.

func TestNew_EmptyDSN(t *testing.T) {
	t.Parallel()
	_, err := postgresadapter.New(context.Background(), "")
	if !errors.Is(err, postgresadapter.ErrEmptyDSN) {
		t.Errorf("err: got %v, want ErrEmptyDSN", err)
	}
}

func TestNew_ParseError(t *testing.T) {
	t.Parallel()
	// pgx.ParseConfig rejects an unknown URL scheme.
	_, err := postgresadapter.New(context.Background(), "ftp://nope")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse dsn") {
		t.Errorf("err: got %v, want wrap with \"parse dsn\"", err)
	}
}

func TestNew_PingFails_WhenUnreachable(t *testing.T) {
	t.Parallel()
	// Port 1 is reserved (TCPMUX) and never listens; pgx will fail to
	// connect well within the context deadline. connect_timeout=1 caps
	// each attempt at the libpq layer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := postgresadapter.New(ctx,
		"postgres://crm:crm@127.0.0.1:1/crm?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("expected connect/ping error, got nil")
	}
	// The error may surface either at connect (NewWithConfig) or at ping;
	// pgxpool is lazy in some versions but the pool will still ping eagerly.
	if !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "ping") {
		t.Errorf("err: got %v, want wrap with \"connect\" or \"ping\"", err)
	}
}

func TestNew_PingsRealPostgres(t *testing.T) {
	t.Parallel()
	ctx := newCtx(t)
	pool, err := postgresadapter.New(ctx, harness.SuperuserDSN())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer pool.Close()
	cfg := pool.Config()
	if cfg.MaxConns != 10 {
		t.Errorf("MaxConns: got %d, want 10", cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("MinConns: got %d, want 2", cfg.MinConns)
	}
	if cfg.MaxConnIdleTime != 5*time.Minute {
		t.Errorf("MaxConnIdleTime: got %v, want 5m", cfg.MaxConnIdleTime)
	}
	if cfg.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime: got %v, want 30m", cfg.MaxConnLifetime)
	}
	if cfg.HealthCheckPeriod != 30*time.Second {
		t.Errorf("HealthCheckPeriod: got %v, want 30s", cfg.HealthCheckPeriod)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Parallel()

	t.Run("nil getenv", func(t *testing.T) {
		t.Parallel()
		_, err := postgresadapter.NewFromEnv(context.Background(), nil)
		if !errors.Is(err, postgresadapter.ErrEmptyDSN) {
			t.Errorf("err: got %v, want ErrEmptyDSN", err)
		}
	})

	t.Run("missing var", func(t *testing.T) {
		t.Parallel()
		_, err := postgresadapter.NewFromEnv(context.Background(), func(string) string { return "" })
		if !errors.Is(err, postgresadapter.ErrEmptyDSN) {
			t.Errorf("err: got %v, want ErrEmptyDSN", err)
		}
	})

	t.Run("populated var connects", func(t *testing.T) {
		t.Parallel()
		ctx := newCtx(t)
		dsn := harness.SuperuserDSN()
		pool, err := postgresadapter.NewFromEnv(ctx, func(name string) string {
			if name == postgresadapter.EnvDSN {
				return dsn
			}
			return ""
		})
		if err != nil {
			t.Fatalf("NewFromEnv: %v", err)
		}
		pool.Close()
	})
}
