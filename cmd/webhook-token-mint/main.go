package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/webhook"
)

// dbConnectTimeout is the bound for `pgxpool.New + Ping`. The CLI
// finishes in ~50 ms over a healthy connection — anything longer means
// the DB is unreachable and the operator should fail fast and retry
// after fixing the network/credentials.
const dbConnectTimeout = 10 * time.Second

// adminBuilder is a seam so realMain can be exercised in tests without
// dialing real Postgres. The production implementation in main()
// constructs a *pgxpool.Pool and wraps it in postgres.NewTokenAdmin.
type adminBuilder func(ctx context.Context, dsn string) (admin webhook.TokenAdmin, closer func(), err error)

// resolveDSNFn is a seam so tests can stub out env/flag DSN resolution.
type resolveDSNFn func(flagDSN string) string

func main() {
	if err := realMain(os.Args[1:], os.Stdout, os.Stderr, defaultResolveDSN, defaultBuildAdmin); err != nil {
		fmt.Fprintf(os.Stderr, "webhook-token-mint: %v\n", err)
		os.Exit(1)
	}
}

// realMain is the test-friendly entry point. It returns the error so
// test code can assert on the exit message without re-parsing stderr.
// All side-effecting dependencies (DSN resolution, DB dial) flow
// through the resolveDSN / buildAdmin function-pointer arguments.
func realMain(
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	resolveDSN resolveDSNFn,
	buildAdmin adminBuilder,
) error {
	fs := flag.NewFlagSet("webhook-token-mint", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		channel    = fs.String("channel", "", "channel name (e.g. whatsapp)")
		tenantID   = fs.String("tenant-id", "", "tenant UUID this token resolves to")
		overlapMin = fs.Int("overlap-minutes", 5, "rotation grace window in minutes (0 = immediate cut)")
		rotateFrom = fs.String("rotate-from-token-hash-hex", "", "if set, schedules revocation of the active row with this hash (hex of sha256 of old plaintext)")
		dsnFlag    = fs.String("dsn", "", "Postgres connection string (defaults to env DATABASE_URL)")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: webhook-token-mint --channel CH --tenant-id UUID [--overlap-minutes N] [--rotate-from-token-hash-hex HEX] [--dsn URL]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn := resolveDSN(*dsnFlag)
	if dsn == "" {
		return fmt.Errorf("--dsn or env DATABASE_URL is required")
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(rootCtx, dbConnectTimeout)
	defer dialCancel()

	admin, closer, err := buildAdmin(dialCtx, dsn)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}

	opts := Options{
		Channel:                *channel,
		TenantID:               *tenantID,
		OverlapMinutes:         *overlapMin,
		RotateFromTokenHashHex: *rotateFrom,
	}
	return Run(rootCtx, admin, webhook.SystemClock{}, opts, stdout, stderr)
}

// defaultResolveDSN reads the --dsn flag value first, falling back to
// the DATABASE_URL env var. Empty string means "neither set".
func defaultResolveDSN(flagDSN string) string {
	if flagDSN != "" {
		return flagDSN
	}
	return os.Getenv("DATABASE_URL")
}

// defaultBuildAdmin is the production seam: open a pgx pool, ping it,
// and wrap it in the admin adapter. Returns a closer that releases
// the pool on CLI exit.
func defaultBuildAdmin(ctx context.Context, dsn string) (webhook.TokenAdmin, func(), error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("ping db: %w", err)
	}
	return postgres.NewTokenAdmin(pool), pool.Close, nil
}
