package circuitbreaker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker"
)

type capturingAlerter struct {
	mu    sync.Mutex
	calls []alertCall
}

type alertCall struct {
	tenantID uuid.UUID
	host     string
	failures int
}

func (a *capturingAlerter) AlertCircuitTripped(_ context.Context, t uuid.UUID, host string, failures int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, alertCall{tenantID: t, host: host, failures: failures})
}

func fixedNow(t time.Time) circuitbreaker.Clock { return func() time.Time { return t } }

func TestBreaker_TripsAtThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	state := circuitbreaker.NewInMemoryState()
	alerter := &capturingAlerter{}
	uc := circuitbreaker.New(state, alerter, fixedNow(now), circuitbreaker.DefaultConfig())
	tenant := uuid.New()

	// 5 failures in a row.
	for i := 1; i < 5; i++ {
		tripped, err := uc.RecordFailure(context.Background(), tenant, "shop.example.com")
		if err != nil {
			t.Fatalf("call %d: err %v", i, err)
		}
		if tripped {
			t.Fatalf("breaker tripped at failure %d; threshold is 5", i)
		}
	}
	tripped, err := uc.RecordFailure(context.Background(), tenant, "shop.example.com")
	if err != nil {
		t.Fatalf("5th failure: %v", err)
	}
	if !tripped {
		t.Fatal("breaker did NOT trip at 5th failure")
	}
	if len(alerter.calls) != 1 {
		t.Fatalf("alerter calls = %d, want 1", len(alerter.calls))
	}
	if alerter.calls[0].failures != 5 || alerter.calls[0].host != "shop.example.com" {
		t.Fatalf("alert payload = %+v", alerter.calls[0])
	}
}

func TestBreaker_IsOpenAfterTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	state := circuitbreaker.NewInMemoryState()
	uc := circuitbreaker.New(state, nil, fixedNow(now), circuitbreaker.DefaultConfig())
	tenant := uuid.New()

	for i := 0; i < 5; i++ {
		_, _ = uc.RecordFailure(context.Background(), tenant, "h")
	}
	open, err := uc.IsOpen(context.Background(), tenant, now)
	if err != nil {
		t.Fatalf("IsOpen err: %v", err)
	}
	if !open {
		t.Fatal("expected breaker open after 5 failures")
	}
	// 24h - 1ms still open.
	if open, _ := uc.IsOpen(context.Background(), tenant, now.Add(24*time.Hour-time.Millisecond)); !open {
		t.Fatal("expected open just before freeze deadline")
	}
	// at deadline, closed.
	if open, _ := uc.IsOpen(context.Background(), tenant, now.Add(24*time.Hour)); open {
		t.Fatal("expected closed at freeze deadline")
	}
}

func TestBreaker_OldFailuresOutsideWindowDoNotTrip(t *testing.T) {
	t.Parallel()
	state := circuitbreaker.NewInMemoryState()
	now := time.Now()
	uc := circuitbreaker.New(state, nil, func() time.Time { return now }, circuitbreaker.DefaultConfig())
	tenant := uuid.New()

	// 4 ancient failures (over 1h ago) — they get evicted.
	old := now.Add(-2 * time.Hour)
	for i := 0; i < 4; i++ {
		uc2 := circuitbreaker.New(state, nil, func() time.Time { return old }, circuitbreaker.DefaultConfig())
		_, _ = uc2.RecordFailure(context.Background(), tenant, "h")
	}
	// 4 fresh failures should not trip (5 threshold).
	for i := 0; i < 4; i++ {
		tripped, _ := uc.RecordFailure(context.Background(), tenant, "h")
		if tripped {
			t.Fatalf("breaker tripped on fresh failure %d; old failures should have aged out", i)
		}
	}
}

func TestBreaker_RecordSuccessClearsTrip(t *testing.T) {
	t.Parallel()
	state := circuitbreaker.NewInMemoryState()
	now := time.Now()
	uc := circuitbreaker.New(state, nil, fixedNow(now), circuitbreaker.DefaultConfig())
	tenant := uuid.New()
	for i := 0; i < 5; i++ {
		_, _ = uc.RecordFailure(context.Background(), tenant, "h")
	}
	if open, _ := uc.IsOpen(context.Background(), tenant, now); !open {
		t.Fatal("precondition: breaker should be open")
	}
	if err := uc.RecordSuccess(context.Background(), tenant); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}
	if open, _ := uc.IsOpen(context.Background(), tenant, now); open {
		t.Fatal("breaker should be closed after RecordSuccess")
	}
}

func TestBreaker_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	uc := circuitbreaker.New(circuitbreaker.NewInMemoryState(), nil, nil, circuitbreaker.DefaultConfig())
	if _, err := uc.RecordFailure(context.Background(), uuid.Nil, "h"); !errors.Is(err, circuitbreaker.ErrZeroTenant) {
		t.Fatalf("err = %v, want ErrZeroTenant", err)
	}
	if _, err := uc.IsOpen(context.Background(), uuid.Nil, time.Now()); !errors.Is(err, circuitbreaker.ErrZeroTenant) {
		t.Fatalf("err = %v, want ErrZeroTenant", err)
	}
	if err := uc.RecordSuccess(context.Background(), uuid.Nil); !errors.Is(err, circuitbreaker.ErrZeroTenant) {
		t.Fatalf("err = %v, want ErrZeroTenant", err)
	}
}

func TestBreaker_ZeroConfigFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	uc := circuitbreaker.New(circuitbreaker.NewInMemoryState(), nil, nil, circuitbreaker.Config{})
	tenant := uuid.New()
	for i := 0; i < 4; i++ {
		_, _ = uc.RecordFailure(context.Background(), tenant, "h")
	}
	tripped, _ := uc.RecordFailure(context.Background(), tenant, "h")
	if !tripped {
		t.Fatal("expected trip with default config")
	}
}

// breakingState fails RecordFailure to cover the error path.
type breakingState struct{ phase string }

func (b *breakingState) RecordFailure(context.Context, uuid.UUID, string, time.Time, time.Duration) (int, error) {
	if b.phase == "record" {
		return 0, errors.New("redis: timeout")
	}
	return 5, nil
}
func (b *breakingState) Trip(context.Context, uuid.UUID, time.Time, time.Duration) error {
	if b.phase == "trip" {
		return errors.New("redis: timeout")
	}
	return nil
}
func (b *breakingState) IsOpen(context.Context, uuid.UUID, time.Time) (bool, error) {
	if b.phase == "isopen" {
		return false, errors.New("redis: timeout")
	}
	return false, nil
}
func (b *breakingState) Reset(context.Context, uuid.UUID) error {
	if b.phase == "reset" {
		return errors.New("redis: timeout")
	}
	return nil
}

func TestBreaker_StateErrorsBubble(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"record", "trip", "isopen", "reset"} {
		phase := phase
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			uc := circuitbreaker.New(&breakingState{phase: phase}, nil, nil, circuitbreaker.DefaultConfig())
			switch phase {
			case "record":
				if _, err := uc.RecordFailure(context.Background(), uuid.New(), "h"); err == nil {
					t.Fatal("expected error from record")
				}
			case "trip":
				if _, err := uc.RecordFailure(context.Background(), uuid.New(), "h"); err == nil {
					t.Fatal("expected error from trip")
				}
			case "isopen":
				if _, err := uc.IsOpen(context.Background(), uuid.New(), time.Now()); err == nil {
					t.Fatal("expected error from IsOpen")
				}
			case "reset":
				if err := uc.RecordSuccess(context.Background(), uuid.New()); err == nil {
					t.Fatal("expected error from Reset")
				}
			}
		})
	}
}
