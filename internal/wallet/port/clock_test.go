package port_test

import (
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// TestSystemClock_Now ensures the wrapper returns a recent timestamp.
func TestSystemClock_Now(t *testing.T) {
	c := port.SystemClock{}
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now %v not within %v..%v", got, before, after)
	}
}

// TestSystemClock_Sleep ensures Sleep blocks for at least the requested
// duration. Use a tiny duration so the test stays fast.
func TestSystemClock_Sleep(t *testing.T) {
	c := port.SystemClock{}
	start := time.Now()
	c.Sleep(5 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 4*time.Millisecond {
		t.Fatalf("Sleep returned too early: %v", elapsed)
	}
}
