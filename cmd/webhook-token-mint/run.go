// Package main is the operator-facing CLI to mint and rotate webhook
// tokens (SIN-62278 / ADR 0075 §2 D1). The plaintext token is shown
// EXACTLY ONCE on stdout; the storage row carries only sha256(plaintext).
//
// run.go isolates the pure "given a TokenAdmin, do the thing" logic so
// that main.go can stay a thin shell over flag parsing + pgx pool
// construction. Tests exercise Run(...) with an in-memory TokenAdmin
// fake that mirrors the partial-unique-index semantics of the real
// table, per the team's "no DB mocks for storage code" rule (testing
// quality bar §5 — same package the rule was originally written for).
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// Options bundles every input the run path consumes. Channel and
// TenantID are required for both Mint and Rotate; the From* fields
// switch the run path into Rotate mode when both are non-zero.
type Options struct {
	// Channel is the registered channel name (e.g. "whatsapp"). The
	// CLI validates it against the same regex Service.RegisterAdapter
	// uses so a typo never reaches the database.
	Channel string

	// TenantID is the canonical UUID string of the tenant the new
	// token will resolve to. Both dashed and undashed UUIDs are
	// accepted via webhook.ParseTenantID.
	TenantID string

	// OverlapMinutes is the rotation grace window. For a fresh mint
	// (no rotate-from), it is stored on the row as metadata only — the
	// new token is active immediately. For a rotation, it is the
	// number of minutes the OLD token stays valid after the new token
	// is minted; 0 means immediate cut.
	OverlapMinutes int

	// RotateFromTokenHashHex is the hex of sha256(old_token). When
	// non-empty the run path shifts the existing active row's
	// revoked_at to now()+overlap_minutes and inserts the new row
	// alongside it. Empty string = pure mint, no rotation.
	RotateFromTokenHashHex string
}

// validate returns nil iff opts is internally consistent. It does not
// attempt to authenticate the operator — that is the deployment's
// responsibility (filesystem perms on the binary, mTLS to the DB).
func (o Options) validate() error {
	if err := webhook.ValidateChannelName(o.Channel); err != nil {
		return fmt.Errorf("--channel: %w", err)
	}
	if _, err := webhook.ParseTenantID(o.TenantID); err != nil {
		return fmt.Errorf("--tenant-id: %w", err)
	}
	if o.OverlapMinutes < 0 {
		return fmt.Errorf("--overlap-minutes must be >= 0, got %d", o.OverlapMinutes)
	}
	if o.RotateFromTokenHashHex != "" {
		if _, err := decodeHashHex(o.RotateFromTokenHashHex); err != nil {
			return fmt.Errorf("--rotate-from-token-hash-hex: %w", err)
		}
	}
	return nil
}

// decodeHashHex parses the operator-supplied hex into a 32-byte slice.
// Whitespace is tolerated (operators paste from a previous mint output).
func decodeHashHex(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if len(clean) != 64 {
		return nil, fmt.Errorf("hash hex must be 64 chars (sha256), got %d", len(clean))
	}
	out, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("not valid hex: %w", err)
	}
	return out, nil
}

// Run executes the mint (or mint+rotate) flow. Output is written to
// out; warnings and errors go to errOut. Plaintext is printed exactly
// once and never returned to the caller, mirroring the
// "you-only-see-it-now" UX the SecurityEngineer asked for.
//
// Returns nil on success; the caller exits 0. Returns a non-nil error
// on validation, generation, or storage failure; the caller exits 1
// without printing the plaintext (because no row was written).
func Run(
	ctx context.Context,
	admin webhook.TokenAdmin,
	clock webhook.Clock,
	opts Options,
	out io.Writer,
	errOut io.Writer,
) error {
	if err := opts.validate(); err != nil {
		return err
	}
	tenantID, err := webhook.ParseTenantID(opts.TenantID)
	if err != nil {
		return fmt.Errorf("parse tenant: %w", err) // unreachable after validate, kept defensive
	}

	plaintext, hash, err := webhook.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}

	now := clock.Now()

	if err := admin.Insert(ctx, tenantID, opts.Channel, hash, opts.OverlapMinutes, now); err != nil {
		// Insert failed — don't print plaintext (no row to attach it
		// to). Surface the typed sentinel so the operator gets a
		// useful message.
		if errors.Is(err, webhook.ErrTokenAlreadyActive) {
			return fmt.Errorf("active token with this hash already exists for channel %q (regen and retry): %w", opts.Channel, err)
		}
		return fmt.Errorf("insert: %w", err)
	}

	rotated := false
	if opts.RotateFromTokenHashHex != "" {
		oldHash, err := decodeHashHex(opts.RotateFromTokenHashHex)
		if err != nil {
			// Already validated, but defensive.
			return fmt.Errorf("decode old hash: %w", err)
		}
		effective := now.Add(time.Duration(opts.OverlapMinutes) * time.Minute)
		if err := admin.ScheduleRevocation(ctx, opts.Channel, oldHash, effective); err != nil {
			// The new row IS already in place. We cannot roll it back
			// without exposing a window where the channel has no
			// active token, so we surface a hard error and tell the
			// operator the new row exists. Operator should DELETE the
			// new row manually if rotation is critical, then retry.
			fmt.Fprintf(errOut, "WARNING: new token row was inserted but the old token's revocation could not be scheduled.\n")
			fmt.Fprintf(errOut, "         New active hash (hex): %s\n", hex.EncodeToString(hash))
			fmt.Fprintf(errOut, "         Run: DELETE FROM webhook_tokens WHERE channel = '%s' AND token_hash = decode('%s', 'hex') AND revoked_at IS NULL;\n", opts.Channel, hex.EncodeToString(hash))
			return fmt.Errorf("schedule revocation of old token: %w", err)
		}
		rotated = true
	}

	// Operator-facing output. No log line carries the plaintext —
	// stdout only — and the caller is expected to redirect away from
	// shell history (e.g. `< /dev/null > token.txt`).
	fmt.Fprintln(out, "===== webhook-token-mint =====")
	fmt.Fprintf(out, "channel:     %s\n", opts.Channel)
	fmt.Fprintf(out, "tenant_id:   %s\n", tenantID.String())
	fmt.Fprintf(out, "hash (hex):  %s\n", hex.EncodeToString(hash))
	if rotated {
		effective := now.Add(time.Duration(opts.OverlapMinutes) * time.Minute)
		fmt.Fprintf(out, "rotation:    OLD %s revokes at %s (overlap = %d min)\n",
			opts.RotateFromTokenHashHex, effective.UTC().Format(time.RFC3339), opts.OverlapMinutes)
	} else {
		fmt.Fprintf(out, "rotation:    none (initial mint, overlap_minutes stored as %d)\n", opts.OverlapMinutes)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "TOKEN PLAINTEXT (copy this NOW — it cannot be retrieved later):")
	fmt.Fprintln(out, plaintext)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "webhook URL: /webhooks/%s/%s\n", opts.Channel, plaintext)
	return nil
}
