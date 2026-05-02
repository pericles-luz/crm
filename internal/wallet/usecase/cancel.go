package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/port"
)

// CancelDebit converts a pending reservation into a cancelled entry.
// Used on the rare rollback path where the LLM call definitely did not
// consume tokens (e.g. local validation rejected the response before
// any provider call).
type CancelDebit struct {
	Repo  port.Repository
	Clock port.Clock
}

// Run cancels the reservation; idempotent w.r.t. already-cancelled
// entries (returns wallet.ErrEntryAlreadyResolved).
func (c CancelDebit) Run(ctx context.Context, entryID string) error {
	if entryID == "" {
		return wallet.ErrEntryNotFound
	}
	if err := c.Repo.Cancel(ctx, entryID, c.Clock.Now()); err != nil {
		if errors.Is(err, wallet.ErrEntryNotFound) ||
			errors.Is(err, wallet.ErrEntryAlreadyResolved) {
			return err
		}
		return fmt.Errorf("wallet/cancel: %w: %w", wallet.ErrTransient, err)
	}
	return nil
}
