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
// package. Same shape as internal/webhook/integration's harness — kept
// independent so the two suites do not race over the same pool.
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

func startHarness(t *testing.T) *harness {
	t.Helper()
	harnessOnce.Do(func() { harnessInst, harnessErr = bootHarness() })
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

func openExternal(dsn, migrationsDir string) (*harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
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
		return nil, fmt.Errorf("pgxpool.New: %w", err)
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

func (h *harness) truncate(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range []string{
		`TRUNCATE TABLE tenant_custom_domains CASCADE`,
		`TRUNCATE TABLE dns_resolution_log CASCADE`,
	} {
		if _, err := h.pool.Exec(ctx, s); err != nil {
			t.Fatalf("truncate %q: %v", s, err)
		}
	}
}

func findMigrationsDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot resolve caller path")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "migrations")
		if matches, _ := filepath.Glob(filepath.Join(candidate, "0010_tenant_custom_domains.up.sql")); len(matches) > 0 {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("migrations directory with 0010_tenant_custom_domains.up.sql not found")
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool, dir, direction string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("invalid direction %q", direction)
	}
	entries, err := os.ReadDir(dir)
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
		for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
			files[i], files[j] = files[j], files[i]
		}
	}
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("exec %s: %w", name, err)
		}
	}
	return nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	if harnessInst != nil && harnessInst.cleanup != nil {
		harnessInst.cleanup()
	}
	os.Exit(code)
}
