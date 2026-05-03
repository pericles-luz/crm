package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/port"
)

// DefaultCommitBackoffs is the exponential backoff schedule mandated by
// AC #2 of SIN-62240: 200ms, 800ms, 3.2s.
//
// AC #2 reads "commit_debit tenta 3x com backoff exponencial …". Read
// that as "three retries between four total attempts": the initial
// attempt fires immediately, then the three Backoffs precede attempts
// 2, 3 and 4. Total attempts therefore equal 1 + len(DefaultCommitBackoffs)
// = 4 and the combined window is roughly 200ms + 800ms + 3200ms = 4.2s.
var DefaultCommitBackoffs = []time.Duration{
	200 * time.Millisecond,
	800 * time.Millisecond,
	3200 * time.Millisecond,
}

// CommitDebit is F37 — the retry-with-backoff commit after the LLM call.
//
// Behaviour: 1 initial attempt + len(Backoffs) retries, with each retry
// preceded by a sleep equal to the corresponding entry in Backoffs
// (so the default 200ms / 800ms / 3.2s schedule covers a 4.2-second
// total window). After all retries fail the entry is handed off to
// the wallet.reconcile_pending queue for the async worker.
//
// Terminal cases short-circuit: ErrEntryNotFound or
// ErrEntryAlreadyResolved return immediately and never enqueue.
type CommitDebit struct {
	Repo     port.Repository
	Queue    port.ReconcileQueue
	Metrics  port.Metrics
	Clock    port.Clock
	Backoffs []time.Duration
}

// Run commits the entry, retrying transient adapter errors per the
// schedule. Caller passes the entry id returned by Reserve.
func (c CommitDebit) Run(ctx context.Context, entryID string) error {
	if entryID == "" {
		return wallet.ErrEntryNotFound
	}
	backoffs := c.Backoffs
	if len(backoffs) == 0 {
		backoffs = DefaultCommitBackoffs
	}

	// total attempts = 1 initial + len(backoffs) retries.
	maxAttempts := 1 + len(backoffs)

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err := c.Repo.Commit(ctx, entryID, c.Clock.Now())
		if err == nil {
			c.Metrics.IncCommitRetry(port.OutcomeSuccess)
			return nil
		}
		lastErr = err
		// Domain-terminal errors do not benefit from retry.
		if errors.Is(err, wallet.ErrEntryNotFound) ||
			errors.Is(err, wallet.ErrEntryAlreadyResolved) {
			c.Metrics.IncCommitRetry(port.OutcomeExhausted)
			return err
		}
		// Best-effort attempt counter for audit.
		_ = c.Repo.IncrementAttempts(ctx, entryID)

		if attempt == maxAttempts {
			break
		}
		c.Metrics.IncCommitRetry(port.OutcomeRetry)
		c.Clock.Sleep(backoffs[attempt-1])
	}

	// All attempts exhausted — hand off to async reconciler.
	wid := ""
	if e, gerr := c.Repo.GetEntry(ctx, entryID); gerr == nil {
		wid = e.WalletID
	}
	job := port.ReconcileJob{EntryID: entryID, WalletID: wid, Attempts: maxAttempts}
	if qerr := c.Queue.Enqueue(ctx, job); qerr != nil {
		c.Metrics.IncCommitRetry(port.OutcomeExhausted)
		return fmt.Errorf("wallet/commit: queue enqueue: %w (last commit err: %v)", qerr, lastErr)
	}
	c.Metrics.IncCommitRetry(port.OutcomeEnqueued)
	return fmt.Errorf("%w: last commit error: %v", wallet.ErrCommitExhausted, lastErr)
}
