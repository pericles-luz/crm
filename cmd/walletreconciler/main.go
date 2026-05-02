//go:build integration

// Command walletreconciler is the nightly job that runs Reconciliator.RunOnce
// against production Postgres. Wire as a cron via routine in Paperclip
// (and in deploy/compose for stg).
//
// Build:  go build -tags integration -o bin/walletreconciler ./cmd/walletreconciler
// Run:    WALLET_PG_DSN=postgres://... bin/walletreconciler
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	prommetrics "github.com/pericles-luz/crm/internal/wallet/adapter/metrics/prom"
	"github.com/pericles-luz/crm/internal/wallet/adapter/openrouter"
	pgrepo "github.com/pericles-luz/crm/internal/wallet/adapter/postgres"
	"github.com/pericles-luz/crm/internal/wallet/adapter/queue/inmem"
	"github.com/pericles-luz/crm/internal/wallet/port"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

type stderrAlerter struct{}

func (stderrAlerter) Send(ctx context.Context, a port.Alert) error {
	log.Printf("ALERT %s: %s — %s (fields=%v)", a.Code, a.Subject, a.Detail, a.Fields)
	return nil
}

func main() {
	dsn := os.Getenv("WALLET_PG_DSN")
	if dsn == "" {
		log.Fatal("WALLET_PG_DSN required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pgxpool: %v", err)
	}
	defer pool.Close()

	r := usecase.Reconciliator{
		Repo:       pgrepo.New(pool),
		Queue:      inmem.New(0),
		Metrics:    prommetrics.New(prometheus.DefaultRegisterer),
		Alerter:    stderrAlerter{},
		Clock:      port.SystemClock{},
		OpenRouter: buildOpenRouter(),
	}
	if err := r.RunOnce(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Fatalf("reconciliator: %v", err)
	}
	log.Println("walletreconciler: pass complete")
}

// buildOpenRouter wires the OpenRouter HTTP adapter when
// OPENROUTER_API_KEY is set; otherwise returns nil so the
// reconciliator skips the inline drift loop. Optional
// OPENROUTER_BASE_URL overrides the default host (used for stg).
func buildOpenRouter() port.OpenRouterCostAPI {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		log.Println("walletreconciler: OPENROUTER_API_KEY not set; OpenRouter drift check disabled")
		return nil
	}
	opts := []openrouter.Option{}
	if base := os.Getenv("OPENROUTER_BASE_URL"); base != "" {
		opts = append(opts, openrouter.WithBaseURL(base))
	}
	return openrouter.New(key, opts...)
}
