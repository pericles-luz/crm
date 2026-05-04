// Package postgres factory for the application's pgx pool.
//
// New is the only place in the codebase allowed to construct a
// *pgxpool.Pool for application use; the testpg harness has its own
// constructor for integration tests. Pool tuning lives here so call sites
// don't need to know the values, and the notenant analyzer (SIN-62232 /
// ADR 0071) blocks any direct .Exec/.Query against the pool from
// non-adapter code — every tenant-scoped query goes through WithTenant.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN names the env var that holds the runtime DSN. cmd/server reads it
// (see PR3 wire-up) and passes the value to NewFromEnv / New.
const EnvDSN = "DATABASE_URL"

// Fase 0 defaults. Tuned for a single-replica app talking to one Postgres;
// PR9 revisits when the production Dockerfile and staging soak land.
const (
	defaultMaxConns          int32         = 10
	defaultMinConns          int32         = 2
	defaultMaxConnIdleTime   time.Duration = 5 * time.Minute
	defaultMaxConnLifetime   time.Duration = 30 * time.Minute
	defaultHealthCheckPeriod time.Duration = 30 * time.Second
)

// ErrEmptyDSN is returned when the DSN string is empty. Callers can use
// errors.Is to surface a startup-time hint (e.g. "set DATABASE_URL").
var ErrEmptyDSN = errors.New("postgres: dsn is empty")

// New parses the DSN, applies the Fase 0 pool defaults, opens the pool, and
// pings to fail fast on bad credentials or unreachable hosts. Callers MUST
// Close the returned pool on shutdown.
//
// The DSN MUST point at the app_runtime role in production. app_runtime is
// NOBYPASSRLS, so SELECTs that don't go through WithTenant return zero rows
// (defense in depth: RLS at the DB plus WithTenant in the app — ADR 0071).
func New(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, ErrEmptyDSN
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	cfg.MaxConns = defaultMaxConns
	cfg.MinConns = defaultMinConns
	cfg.MaxConnIdleTime = defaultMaxConnIdleTime
	cfg.MaxConnLifetime = defaultMaxConnLifetime
	cfg.HealthCheckPeriod = defaultHealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// NewFromEnv is the convenience wrapper used by cmd/server. It reads
// DATABASE_URL via the supplied getenv (typically os.Getenv) and forwards
// to New. Returning ErrEmptyDSN here lets the caller log a deterministic
// "DATABASE_URL is not set" message without sniffing the wrap chain.
func NewFromEnv(ctx context.Context, getenv func(string) string) (*pgxpool.Pool, error) {
	if getenv == nil {
		return nil, ErrEmptyDSN
	}
	return New(ctx, getenv(EnvDSN))
}
