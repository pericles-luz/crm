package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/port"
)

// DefaultWorkerBackoffs is the schedule the async worker uses between
// retries on a single dequeued job. Slower than the inline retry
// because a job that already failed 3x inline is unlikely to recover
// in milliseconds.
var DefaultWorkerBackoffs = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
}

// DefaultWorkerEscalateAfter is the worker-side attempt count above
// which the worker emits a wallet.commit_persistent_failure alert. The
// counter is local to a single dequeued job: escalation is NOT gated on
// the cumulative job.Attempts, because that would let a job arriving
// already at-or-above the threshold alert before the worker ever
// retried (reviewer smaller-finding 2 on SIN-62240).
const DefaultWorkerEscalateAfter = 5

// ReconcileWorker drains the wallet.reconcile_pending queue and tries
// to commit each pending entry, escalating to the alerter after
// EscalateAfter worker-side attempts on the same job have failed.
//
// One worker instance owns one consumer goroutine. The Run method
// blocks until ctx is cancelled or the queue returns a non-cancellation
// error.
type ReconcileWorker struct {
	Repo          port.Repository
	Queue         port.ReconcileQueue
	Metrics       port.Metrics
	Alerter       port.Alerter
	Clock         port.Clock
	Backoffs      []time.Duration // attempt N waits Backoffs[N-1] before retry
	EscalateAfter int             // worker-side attempts before alert (0 → DefaultWorkerEscalateAfter)
}

// Run blocks consuming jobs until ctx is done.
func (w ReconcileWorker) Run(ctx context.Context) error {
	for {
		job, err := w.Queue.Dequeue(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("wallet/worker: dequeue: %w", err)
		}
		w.handle(ctx, job)
	}
}

// handle processes a single job. Exposed so RunOnce can drive the
// worker step-wise from tests without spawning a goroutine.
func (w ReconcileWorker) handle(ctx context.Context, job port.ReconcileJob) {
	backoffs := w.Backoffs
	if len(backoffs) == 0 {
		backoffs = DefaultWorkerBackoffs
	}
	escalateAfter := w.EscalateAfter
	if escalateAfter <= 0 {
		escalateAfter = DefaultWorkerEscalateAfter
	}

	// workerAttempts counts how many times THIS worker invocation has
	// tried Commit on this job. It starts at 0 and is independent of
	// the cumulative job.Attempts — see DefaultWorkerEscalateAfter.
	// totalAttempts is the audit counter (cumulative inline + worker)
	// reported in alerts and persisted via IncrementAttempts.
	workerAttempts := 0
	totalAttempts := job.Attempts
	for i := 0; i < len(backoffs); i++ {
		if ctx.Err() != nil {
			return
		}
		err := w.Repo.Commit(ctx, job.EntryID, w.Clock.Now())
		workerAttempts++
		totalAttempts++
		if err == nil {
			w.Metrics.IncCommitRetry(port.OutcomeSuccess)
			return
		}
		// Domain terminal errors are not retryable.
		if errors.Is(err, wallet.ErrEntryNotFound) ||
			errors.Is(err, wallet.ErrEntryAlreadyResolved) {
			w.Metrics.IncCommitRetry(port.OutcomeExhausted)
			return
		}
		w.Metrics.IncCommitRetry(port.OutcomeRetry)
		_ = w.Repo.IncrementAttempts(ctx, job.EntryID)

		// Escalate strictly on worker-side attempts so a job that
		// arrives already over the inline-retry threshold still gets
		// a real retry budget here.
		if workerAttempts >= escalateAfter {
			_ = w.Alerter.Send(ctx, port.Alert{
				Code:    "wallet.commit_persistent_failure",
				Subject: "Wallet commit retry threshold exceeded",
				Detail:  fmt.Sprintf("entry %s on wallet %s — %d worker attempts (total %d)", job.EntryID, job.WalletID, workerAttempts, totalAttempts),
				Fields: map[string]string{
					"entry_id":        job.EntryID,
					"wallet_id":       job.WalletID,
					"attempts":        fmt.Sprintf("%d", totalAttempts),
					"worker_attempts": fmt.Sprintf("%d", workerAttempts),
				},
			})
			w.Metrics.IncCommitRetry(port.OutcomeExhausted)
			return
		}

		if i == len(backoffs)-1 {
			break
		}
		w.Clock.Sleep(backoffs[i])
	}
}

// RunOnce drains jobs already enqueued and exits when the queue would
// block. Used by tests and by short-lived CLI runners. Not safe to use
// from production wiring (use Run with a long-lived ctx instead).
func (w ReconcileWorker) RunOnce(ctx context.Context) {
	for {
		dctx, cancel := context.WithTimeout(ctx, 1*time.Millisecond)
		job, err := w.Queue.Dequeue(dctx)
		cancel()
		if err != nil {
			return
		}
		w.handle(ctx, job)
	}
}
