package enrollment_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
)

type fakeStore struct {
	count int
	err   error
}

func (s *fakeStore) ActiveCount(context.Context, uuid.UUID) (int, error) {
	return s.count, s.err
}

type fakeCounter struct {
	mu     sync.Mutex
	counts map[enrollment.Window]int
	err    error
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{counts: map[enrollment.Window]int{}}
}

func (c *fakeCounter) CountAndRecord(_ context.Context, _ uuid.UUID, w enrollment.Window, _ time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	c.counts[w]++
	return c.counts[w], nil
}

func (c *fakeCounter) seed(w enrollment.Window, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[w] = n
}

type fakeBreaker struct {
	open bool
	err  error
}

func (b *fakeBreaker) IsOpen(context.Context, uuid.UUID, time.Time) (bool, error) {
	return b.open, b.err
}

type capturingAudit struct {
	mu       sync.Mutex
	tenants  []uuid.UUID
	deciSion []enrollment.Decision
	reasons  []string
}

func (a *capturingAudit) LogEnrollmentDecision(_ context.Context, t uuid.UUID, d enrollment.Decision, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tenants = append(a.tenants, t)
	a.deciSion = append(a.deciSion, d)
	a.reasons = append(a.reasons, reason)
}

func fixedNow(now time.Time) enrollment.Clock {
	return func() time.Time { return now }
}

func TestEnrollment_AllowsWhenAllQuotasUnderLimit(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	uc := enrollment.New(
		&fakeStore{count: 1},
		newFakeCounter(),
		nil,
		nil,
		fixedNow(time.Now()),
		enrollment.DefaultQuota(),
	)
	got := uc.Allow(context.Background(), tenant)
	if got.Decision != enrollment.DecisionAllowed {
		t.Fatalf("decision = %v, want allowed", got.Decision)
	}
}

func TestEnrollment_HardCapDeniesWithoutRedis(t *testing.T) {
	t.Parallel()
	counter := newFakeCounter()
	uc := enrollment.New(
		&fakeStore{count: 25},
		counter,
		nil, nil, nil,
		enrollment.DefaultQuota(),
	)
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionDeniedHardCap {
		t.Fatalf("decision = %v, want hard_cap", got.Decision)
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	for w, n := range counter.counts {
		if n != 0 {
			t.Fatalf("counter incremented %v=%d after hard-cap deny", w, n)
		}
	}
}

func TestEnrollment_HourlyQuotaDenies(t *testing.T) {
	t.Parallel()
	counter := newFakeCounter()
	counter.seed(enrollment.WindowHour, 5) // pre-existing, next call → 6 > 5
	uc := enrollment.New(&fakeStore{}, counter, nil, nil, nil, enrollment.DefaultQuota())
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionDeniedHourlyQuota {
		t.Fatalf("decision = %v, want hourly_quota", got.Decision)
	}
	if got.ResetAfter <= 0 {
		t.Fatalf("ResetAfter = %v, want > 0", got.ResetAfter)
	}
}

func TestEnrollment_DailyQuotaDeniesAfterHourly(t *testing.T) {
	t.Parallel()
	counter := newFakeCounter()
	counter.seed(enrollment.WindowDay, 20)
	uc := enrollment.New(&fakeStore{}, counter, nil, nil, nil, enrollment.DefaultQuota())
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionDeniedDailyQuota {
		t.Fatalf("decision = %v, want daily_quota", got.Decision)
	}
}

func TestEnrollment_MonthlyQuotaDenies(t *testing.T) {
	t.Parallel()
	counter := newFakeCounter()
	counter.seed(enrollment.WindowMonth, 50)
	uc := enrollment.New(&fakeStore{}, counter, nil, nil, nil, enrollment.DefaultQuota())
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionDeniedMonthlyQuota {
		t.Fatalf("decision = %v, want monthly_quota", got.Decision)
	}
}

func TestEnrollment_CircuitBreakerOpenDenies(t *testing.T) {
	t.Parallel()
	uc := enrollment.New(
		&fakeStore{},
		newFakeCounter(),
		&fakeBreaker{open: true},
		nil, nil,
		enrollment.DefaultQuota(),
	)
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionDeniedCircuitBreaker {
		t.Fatalf("decision = %v, want circuit_breaker", got.Decision)
	}
}

func TestEnrollment_AuditCapturesEveryDecision(t *testing.T) {
	t.Parallel()
	audit := &capturingAudit{}
	uc := enrollment.New(&fakeStore{}, newFakeCounter(), nil, audit, nil, enrollment.DefaultQuota())
	tenant := uuid.New()
	uc.Allow(context.Background(), tenant)
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.tenants) != 1 || audit.tenants[0] != tenant {
		t.Fatalf("audit tenants = %v", audit.tenants)
	}
	if audit.deciSion[0] != enrollment.DecisionAllowed {
		t.Fatalf("audit decision = %v, want allowed", audit.deciSion[0])
	}
	if audit.reasons[0] != "allowed" {
		t.Fatalf("audit reason = %v, want allowed", audit.reasons[0])
	}
}

func TestEnrollment_StoreErrorBubblesAsError(t *testing.T) {
	t.Parallel()
	boom := errors.New("pg: timeout")
	uc := enrollment.New(&fakeStore{err: boom}, newFakeCounter(), nil, nil, nil, enrollment.DefaultQuota())
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionError {
		t.Fatalf("decision = %v, want error", got.Decision)
	}
	if !errors.Is(got.Err, boom) {
		t.Fatalf("err = %v, want wrap of %v", got.Err, boom)
	}
}

func TestEnrollment_CounterErrorBubbles(t *testing.T) {
	t.Parallel()
	boom := errors.New("redis: timeout")
	c := newFakeCounter()
	c.err = boom
	uc := enrollment.New(&fakeStore{}, c, nil, nil, nil, enrollment.DefaultQuota())
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionError {
		t.Fatalf("decision = %v, want error", got.Decision)
	}
}

func TestEnrollment_BreakerErrorBubbles(t *testing.T) {
	t.Parallel()
	boom := errors.New("breaker: timeout")
	uc := enrollment.New(&fakeStore{}, newFakeCounter(), &fakeBreaker{err: boom}, nil, nil, enrollment.DefaultQuota())
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionError {
		t.Fatalf("decision = %v, want error", got.Decision)
	}
}

func TestEnrollment_ZeroTenantRejected(t *testing.T) {
	t.Parallel()
	uc := enrollment.New(&fakeStore{}, newFakeCounter(), nil, nil, nil, enrollment.DefaultQuota())
	got := uc.Allow(context.Background(), uuid.Nil)
	if got.Decision != enrollment.DecisionError {
		t.Fatalf("decision = %v, want error", got.Decision)
	}
	if !errors.Is(got.Err, enrollment.ErrZeroTenant) {
		t.Fatalf("err = %v, want ErrZeroTenant", got.Err)
	}
}

func TestEnrollment_ZeroQuotaFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	uc := enrollment.New(&fakeStore{count: 25}, newFakeCounter(), nil, nil, nil, enrollment.Quota{})
	got := uc.Allow(context.Background(), uuid.New())
	if got.Decision != enrollment.DecisionDeniedHardCap {
		t.Fatalf("decision = %v, want hard_cap (default 25)", got.Decision)
	}
}

func TestEnrollment_DecisionAndWindowStrings(t *testing.T) {
	t.Parallel()
	dcases := map[enrollment.Decision]string{
		enrollment.DecisionAllowed:              "allowed",
		enrollment.DecisionDeniedHardCap:        "hard_cap",
		enrollment.DecisionDeniedHourlyQuota:    "hourly_quota",
		enrollment.DecisionDeniedDailyQuota:     "daily_quota",
		enrollment.DecisionDeniedMonthlyQuota:   "monthly_quota",
		enrollment.DecisionDeniedCircuitBreaker: "circuit_breaker",
		enrollment.DecisionError:                "error",
		enrollment.DecisionUnknown:              "unknown",
	}
	for d, s := range dcases {
		if got := d.String(); got != s {
			t.Errorf("Decision(%d).String() = %q, want %q", d, got, s)
		}
	}
	wcases := map[enrollment.Window]string{
		enrollment.WindowHour:  "hour",
		enrollment.WindowDay:   "day",
		enrollment.WindowMonth: "month",
		enrollment.Window(99):  "unknown",
	}
	for w, s := range wcases {
		if got := w.String(); got != s {
			t.Errorf("Window(%d).String() = %q, want %q", w, got, s)
		}
	}
	dcases2 := map[enrollment.Window]time.Duration{
		enrollment.WindowHour:  time.Hour,
		enrollment.WindowDay:   24 * time.Hour,
		enrollment.WindowMonth: 30 * 24 * time.Hour,
		enrollment.Window(99):  time.Hour,
	}
	for w, d := range dcases2 {
		if got := w.Duration(); got != d {
			t.Errorf("Window(%d).Duration() = %v, want %v", w, got, d)
		}
	}
}
