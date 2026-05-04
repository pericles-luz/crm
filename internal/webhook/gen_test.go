package webhook_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/pericles-luz/crm/internal/webhook"
)

// TestGenerateToken_Shape pins the surface contract: 32 bytes of
// entropy → 64 hex chars plaintext, 32-byte sha256 hash. The lookup
// path in service.go relies on hash being exactly sha256(plaintext);
// regressing that breaks every authenticated webhook.
func TestGenerateToken_Shape(t *testing.T) {
	t.Parallel()
	pt, hash, err := webhook.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if got, want := len(pt), 2*webhook.TokenByteLen; got != want {
		t.Fatalf("plaintext len = %d, want %d (hex of %d bytes)", got, want, webhook.TokenByteLen)
	}
	if _, err := hex.DecodeString(pt); err != nil {
		t.Fatalf("plaintext is not hex: %v", err)
	}
	if got, want := len(hash), sha256.Size; got != want {
		t.Fatalf("hash len = %d, want %d", got, want)
	}
	expect := sha256.Sum256([]byte(pt))
	for i := range hash {
		if hash[i] != expect[i] {
			t.Fatalf("hash[%d]=%x, expected sha256(plaintext)[%d]=%x — mint hash MUST equal lookup hash", i, hash[i], i, expect[i])
		}
	}
}

// TestGenerateToken_Uniqueness gives a smoke-level guarantee that two
// successive mints don't collide. With 256-bit entropy a real collision
// is astronomical; this test mostly catches accidental "use a fixed
// seed" regressions if someone refactors gen.go to use math/rand.
func TestGenerateToken_Uniqueness(t *testing.T) {
	t.Parallel()
	const N = 64
	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		pt, _, err := webhook.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken[%d]: %v", i, err)
		}
		if _, dup := seen[pt]; dup {
			t.Fatalf("plaintext %q minted twice in %d attempts", pt, N)
		}
		seen[pt] = struct{}{}
	}
}

// TestGenerateToken_NotAllZeroes guards against a regression where
// `make([]byte, 32)` is hex-encoded without ever calling rand.Read —
// the legitimate failure mode if someone replaces crypto/rand.Read with
// a stub. The probability of crypto/rand.Read returning all-zero in a
// healthy environment is 2^-256.
func TestGenerateToken_NotAllZeroes(t *testing.T) {
	t.Parallel()
	pt, hash, err := webhook.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	allZero := true
	for i := 0; i < len(pt); i++ {
		if pt[i] != '0' {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatalf("plaintext is 64 zero hex digits — entropy source is dead")
	}
	allZero = true
	for _, b := range hash {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatalf("hash is all zero — mint pipeline is broken")
	}
}
