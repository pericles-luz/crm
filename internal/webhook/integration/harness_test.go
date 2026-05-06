//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// harness owns the Postgres connection used by every test in this
// package. There is exactly one instance per `go test` process, lazily
// initialized on the first call to startHarness. Per-test schema state
// is reset via the truncate helper rather than recreating the database
// (faster and exercises the production schema, not a per-test variant).
type harness struct {
	pool          *pgxpool.Pool
	dsn           string
	migrationsDir string
	cleanup       func()
}

var (
	harnessOnce sync.Once
	harnessInst *harness
	harnessErr  error
)

// startHarness ensures Postgres is reachable, applies the 0075a..0075d
// migrations, and returns the shared harness. Idempotent: callers can
// invoke it from every TestXxx function.
func startHarness(t *testing.T) *harness {
	t.Helper()
	harnessOnce.Do(func() {
		harnessInst, harnessErr = bootHarness()
	})
	if harnessErr != nil {
		t.Fatalf("integration harness: %v", harnessErr)
	}
	return harnessInst
}

func bootHarness() (*harness, error) {
	migrationsDir, err := findMigrationsDir()
	if err != nil {
		return nil, err
	}

	if dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN")); dsn != "" {
		return openExternal(dsn, migrationsDir)
	}
	return startContainer(migrationsDir)
}

// openExternal opens a pool against an externally provided DSN. The
// caller is responsible for the database being reachable and empty
// enough that migrations can be applied. The harness drops every object
// it creates on cleanup so re-running against a shared DB is safe.
func openExternal(dsn, migrationsDir string) (*harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %q: %w", dsn, err)
	}
	h := &harness{
		pool:          pool,
		dsn:           dsn,
		migrationsDir: migrationsDir,
		cleanup: func() {
			_ = applyMigrations(context.Background(), pool, migrationsDir, "down")
			pool.Close()
		},
	}
	if err := applyMigrations(ctx, pool, migrationsDir, "up"); err != nil {
		h.cleanup()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return h, nil
}

// startContainer boots a postgres:16-alpine testcontainer and applies
// the migrations.
func startContainer(migrationsDir string) (*harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("crmtest"),
		tcpostgres.WithUsername("crm"),
		tcpostgres.WithPassword("crm"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("testcontainers postgres: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(context.Background())
		return nil, fmt.Errorf("container DSN: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = container.Terminate(context.Background())
		return nil, fmt.Errorf("pgxpool.New (container): %w", err)
	}

	h := &harness{
		pool:          pool,
		dsn:           dsn,
		migrationsDir: migrationsDir,
		cleanup: func() {
			pool.Close()
			_ = container.Terminate(context.Background())
		},
	}
	if err := applyMigrations(ctx, pool, migrationsDir, "up"); err != nil {
		h.cleanup()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return h, nil
}

// truncate clears all webhook-table rows so the next test starts from
// a known state. Schema (tables, indexes, partitions) is preserved.
// Partition tables for raw_event are also truncated by virtue of
// TRUNCATE on the parent.
func (h *harness) truncate(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stmts := []string{
		`TRUNCATE TABLE webhook_idempotency`,
		`TRUNCATE TABLE webhook_tokens`,
		`TRUNCATE TABLE tenant_channel_associations`,
		`TRUNCATE TABLE raw_event`,
	}
	for _, s := range stmts {
		if _, err := h.pool.Exec(ctx, s); err != nil {
			t.Fatalf("truncate %q: %v", s, err)
		}
	}
}

// findMigrationsDir walks up from the current source file's directory
// until it finds a sibling `migrations/` containing 0075a*.up.sql.
func findMigrationsDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot resolve caller path")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "migrations")
		if matches, _ := filepath.Glob(filepath.Join(candidate, "0075a_webhook_tokens.up.sql")); len(matches) > 0 {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("migrations directory with 0075a_webhook_tokens.up.sql not found")
}

// applyMigrations applies all *.up.sql files in lexical order when
// direction=="up", or all *.down.sql files in reverse lexical order when
// direction=="down". Files are read from migrationsDir and executed
// inside a single transaction per file (matches goose semantics for the
// migrations we ship — none of them use CREATE INDEX CONCURRENTLY).
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, migrationsDir, direction string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("invalid migration direction %q", direction)
	}
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return err
	}
	suffix := "." + direction + ".sql"
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	if direction == "down" {
		// Reverse for rollback so 0075d falls before 0075a.
		for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
			files[i], files[j] = files[j], files[i]
		}
	}
	for _, name := range files {
		path := filepath.Join(migrationsDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("exec %s: %w", name, err)
		}
	}
	return nil
}

// TestMain ensures container/pool resources are released even when the
// process is interrupted between tests.
func TestMain(m *testing.M) {
	code := m.Run()
	if harnessInst != nil && harnessInst.cleanup != nil {
		harnessInst.cleanup()
	}
	os.Exit(code)
}
