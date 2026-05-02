package wallet_test

import (
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/wallet"
)

func TestLedgerEntry_Validate(t *testing.T) {
	cases := []struct {
		name    string
		entry   wallet.LedgerEntry
		wantErr error
	}{
		{
			name: "valid debit",
			entry: wallet.LedgerEntry{
				Amount: 100, Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Status: wallet.StatusPending,
			},
		},
		{
			name: "valid credit",
			entry: wallet.LedgerEntry{
				Amount: 1, Kind: wallet.KindCredit, Source: wallet.SourceGrant, Status: wallet.StatusPosted,
			},
		},
		{
			name:    "zero amount",
			entry:   wallet.LedgerEntry{Amount: 0, Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Status: wallet.StatusPending},
			wantErr: wallet.ErrInvalidAmount,
		},
		{
			name:    "negative amount",
			entry:   wallet.LedgerEntry{Amount: -1, Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Status: wallet.StatusPending},
			wantErr: wallet.ErrInvalidAmount,
		},
		{
			name:    "bad kind",
			entry:   wallet.LedgerEntry{Amount: 1, Kind: "weird", Source: wallet.SourceLLMCall, Status: wallet.StatusPending},
			wantErr: wallet.ErrInvalidKind,
		},
		{
			name:    "bad source",
			entry:   wallet.LedgerEntry{Amount: 1, Kind: wallet.KindDebit, Source: "weird", Status: wallet.StatusPending},
			wantErr: wallet.ErrInvalidSource,
		},
		{
			name:    "bad status",
			entry:   wallet.LedgerEntry{Amount: 1, Kind: wallet.KindDebit, Source: wallet.SourceLLMCall, Status: "weird"},
			wantErr: wallet.ErrInvalidStatus,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entry.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLedgerEntry_SignedAmount(t *testing.T) {
	debit := wallet.LedgerEntry{Amount: 10, Kind: wallet.KindDebit}
	credit := wallet.LedgerEntry{Amount: 10, Kind: wallet.KindCredit}
	if got := debit.SignedAmount(); got != -10 {
		t.Errorf("debit signed: got %d, want -10", got)
	}
	if got := credit.SignedAmount(); got != 10 {
		t.Errorf("credit signed: got %d, want 10", got)
	}
	if !debit.IsPending() && debit.Status == wallet.StatusPending {
		t.Errorf("IsPending mismatch")
	}
	debit.Status = wallet.StatusPending
	if !debit.IsPending() {
		t.Errorf("IsPending false-negative")
	}
}
