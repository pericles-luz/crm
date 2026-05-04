package metrics_test

import (
	"sync"
	"testing"

	"github.com/pericles-luz/crm/internal/ratelimit/metrics"
)

func TestNoop_Implements(t *testing.T) {
	t.Parallel()
	var r metrics.Recorder = metrics.Noop{}
	r.Allowed("e", "b")
	r.Denied("e", "b")
	r.Unavailable("e")
}

func TestCounter_TracksLabels(t *testing.T) {
	t.Parallel()
	c := metrics.NewCounter()
	c.Allowed("/login", "ip")
	c.Allowed("/login", "ip")
	c.Allowed("/login", "email")
	c.Denied("/login", "email")
	c.Unavailable("/login")
	c.Unavailable("/login")

	if got := c.AllowedCount("/login", "ip"); got != 2 {
		t.Fatalf("Allowed(/login,ip) = %d, want 2", got)
	}
	if got := c.AllowedCount("/login", "email"); got != 1 {
		t.Fatalf("Allowed(/login,email) = %d, want 1", got)
	}
	if got := c.DeniedCount("/login", "email"); got != 1 {
		t.Fatalf("Denied(/login,email) = %d, want 1", got)
	}
	if got := c.UnavailableCount("/login"); got != 2 {
		t.Fatalf("Unavailable(/login) = %d, want 2", got)
	}
	if got := c.AllowedCount("/login", "missing"); got != 0 {
		t.Fatalf("absent bucket should read zero, got %d", got)
	}
}

func TestCounter_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	c := metrics.NewCounter()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Allowed("/x", "b")
			}
		}()
	}
	wg.Wait()
	if got := c.AllowedCount("/x", "b"); got != 1000 {
		t.Fatalf("concurrent Allowed = %d, want 1000", got)
	}
}
