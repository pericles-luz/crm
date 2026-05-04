// Token minting helpers for the operator-facing CLI (cmd/webhook-token-mint).
//
// SIN-62278 / ADR 0075 §2 D1 + F-10:
//
//   - The plaintext token is 32 bytes (256 bits) read from crypto/rand,
//     hex-encoded so it survives URL paths and operator copy/paste without
//     needing percent-encoding.
//   - Only the SHA-256 of the plaintext is persisted in webhook_tokens.
//     The plaintext is shown to the operator exactly once at mint time and
//     never recoverable thereafter.
//   - F-10: math/rand MUST NOT be imported here, nor anywhere under
//     internal/webhook/, nor in any *gen.go file. The paperclip-lint
//     "nomathrand" analyzer enforces that. Reviewers should reject any PR
//     that bypasses crypto/rand for token material.
//
// This file lives in the domain package on purpose: gen.go is a leaf
// helper with no port dependencies, the lint scope was chosen to cover it,
// and pulling it into a sub-package would split the "all token plumbing
// in one place" mental model the SecurityEngineer asked for in the
// SIN-62234 re-review.

package webhook

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// TokenByteLen is the entropy size used for new webhook tokens, in
// bytes. 32 bytes = 256 bits CSPRNG, matches ADR 0075 §2 D1.
const TokenByteLen = 32

// GenerateToken returns a freshly minted token. Plaintext is the
// hex-encoded form delivered to the operator; hash is sha256(plaintext)
// — the exact same bytes the service computes for the URL token in
// service.go, so a token minted here will hit the active partial-unique
// index `(channel, token_hash) WHERE revoked_at IS NULL` declared in
// migration 0075a.
//
// crypto/rand.Read is documented to return an error only on a critical
// OS failure; we surface it instead of swallowing because the CLI exit
// code is the operator's signal that no token was minted.
func GenerateToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, TokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("webhook: rand.Read: %w", err)
	}
	pt := hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(pt))
	return pt, sum[:], nil
}
