package usecase

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// DefaultPendingThreshold is how old a pending entry must be before
// the nightly reconciliator considers it "stuck".
const DefaultPendingThreshold = 30 * time.Minute

// DefaultDriftAlertPct is the wallet drift threshold above which the
// reconciliator emits wallet.reconciliation_drift (AC #4 = 1%).
const DefaultDriftAlertPct = 0.01

// DefaultOpenRouterDriftAlertPct is the OpenRouter drift threshold
// (AC #5 = 5%).
const DefaultOpenRouterDriftAlertPct = 0.05

// Reconciliator is F37 acceptance criteria #3 (worker retry) +
// #4 (nightly drift) + #5 (OpenRouter drift, when an adapter is
// supplied). It is a use case driven by a routine cron.
type Reconciliator struct {
	Repo                  port.Repository
	Queue                 port.ReconcileQueue
	Metrics               port.Metrics
	Alerter               port.Alerter
	Clock                 port.Clock
	OpenRouter            port.OpenRouterCostAPI // optional; nil disables external drift check
	PendingThreshold      time.Duration
	DriftAlertPct         float64
	OpenRouterAlertPct    float64
}

// RunOnce performs one reconciliation pass:
//   1. Update wallet_pending_entries_gauge.
//   2. Re-enqueue pending entries older than PendingThreshold so the
//      async worker rescues them.
//   3. For each wallet, compute drift between Σ posted ledger and
//      balance_movement; alert/metric if drift > DriftAlertPct.
//   4. If OpenRouter is set, fetch yesterday's external usage per
//      wallet master and alert if drift > OpenRouterAlertPct.
//
// Returns the first error encountered but always finishes the pass.
func (r Reconciliator) RunOnce(ctx context.Context) error {
	pendingThreshold := r.PendingThreshold
	if pendingThreshold <= 0 {
		pendingThreshold = DefaultPendingThreshold
	}
	driftPct := r.DriftAlertPct
	if driftPct <= 0 {
		driftPct = DefaultDriftAlertPct
	}
	orPct := r.OpenRouterAlertPct
	if orPct <= 0 {
		orPct = DefaultOpenRouterDriftAlertPct
	}
	now := r.Clock.Now()

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 1) gauge of pending entries
	if cnt, err := r.Repo.CountPending(ctx); err == nil {
		r.Metrics.SetPendingEntries(cnt)
	} else {
		keep(err)
	}

	// 2) rescue stuck pending entries
	stuck, err := r.Repo.ListPendingOlderThan(ctx, now.Add(-pendingThreshold))
	keep(err)
	for _, e := range stuck {
		_ = r.Queue.Enqueue(ctx, port.ReconcileJob{EntryID: e.ID, WalletID: e.WalletID, Attempts: int(e.Attempts)})
	}

	// 3) wallet drift — reconciles posted ledger against balance_movement
	//
	// Invariant in steady state (no pending entries):
	//   sum_posted_signed + (initial_balance - balance_movement) == 0
	// because Reserve subtracts amount from balance_movement (debit) and
	// Commit posts the same signed amount to the ledger. Drift is the
	// magnitude of that residual normalised by initial_balance.
	wallets, listErr := r.Repo.ListWallets(ctx)
	keep(listErr)
	activeWalletIDs := make([]string, 0, len(wallets))
	activeMasterIDs := make([]string, 0, len(wallets))
	for _, w := range wallets {
		activeWalletIDs = append(activeWalletIDs, w.ID)
		activeMasterIDs = append(activeMasterIDs, w.MasterID)
		sum, err := r.Repo.SumPostedByWallet(ctx, w.ID)
		if err != nil {
			keep(err)
			continue
		}
		residual := sum + (w.InitialBalance - w.BalanceMovement)
		denom := w.InitialBalance
		if denom < 1 {
			denom = 1
		}
		drift := absInt64(residual)
		driftFraction := float64(drift) / float64(denom)
		r.Metrics.SetReconciliationDriftPct(w.ID, driftFraction)
		if driftFraction > driftPct {
			_ = r.Alerter.Send(ctx, port.Alert{
				Code:    "wallet.reconciliation_drift",
				Subject: fmt.Sprintf("Wallet %s drift %.2f%%", w.ID, driftFraction*100),
				Detail:  fmt.Sprintf("ledger sum %d, balance_movement %d, initial_balance %d", sum, w.BalanceMovement, w.InitialBalance),
				Fields: map[string]string{
					"wallet_id":        w.ID,
					"master_id":        w.MasterID,
					"ledger_sum":       fmt.Sprintf("%d", sum),
					"balance_movement": fmt.Sprintf("%d", w.BalanceMovement),
					"initial_balance":  fmt.Sprintf("%d", w.InitialBalance),
					"drift_pct":        fmt.Sprintf("%.4f", driftFraction),
				},
			})
		}
	}
	// Prune drift label series for wallets that fell out of ListWallets,
	// otherwise the Prometheus registry leaks dead label sets forever
	// proportional to wallet churn (SIN-62269). Only prune when the list
	// itself succeeded — a transient ListWallets error must not delete
	// labels for still-live wallets.
	if listErr == nil {
		r.Metrics.RetainReconciliationDriftLabels(activeWalletIDs)
	}

	// 4) OpenRouter external drift (optional)
	if r.OpenRouter != nil {
		yesterday := now.Add(-24 * time.Hour).UTC().Truncate(24 * time.Hour)
		for _, w := range wallets {
			sample, err := r.OpenRouter.DailyUsage(ctx, w.MasterID, yesterday)
			if err != nil {
				keep(err)
				continue
			}
			internal, err := r.Repo.SumPostedByWallet(ctx, w.ID)
			if err != nil {
				keep(err)
				continue
			}
			drift := computeDriftPct(-internal, sample.Tokens) // -internal because debits are negative
			r.Metrics.SetOpenRouterDriftPct(w.MasterID, drift)
			if drift > orPct {
				_ = r.Alerter.Send(ctx, port.Alert{
					Code:    "wallet.openrouter_drift",
					Subject: fmt.Sprintf("OpenRouter drift %.2f%% for master %s", drift*100, w.MasterID),
					Detail:  fmt.Sprintf("openrouter %d vs ledger %d", sample.Tokens, -internal),
					Fields: map[string]string{
						"master_id":   w.MasterID,
						"openrouter":  fmt.Sprintf("%d", sample.Tokens),
						"ledger":      fmt.Sprintf("%d", -internal),
						"drift_pct":   fmt.Sprintf("%.4f", drift),
					},
				})
			}
		}
		if listErr == nil {
			r.Metrics.RetainOpenRouterDriftLabels(activeMasterIDs)
		}
	}
	return firstErr
}

// computeDriftPct returns |a - b| / max(|b|, 1) — used by the OpenRouter
// drift comparison where the two values are independent measurements
// of the same underlying quantity.
func computeDriftPct(a, b int64) float64 {
	abs := math.Abs(float64(a - b))
	denom := math.Abs(float64(b))
	if denom < 1 {
		denom = 1
	}
	return abs / denom
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
