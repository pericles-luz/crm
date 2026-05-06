// Package enrollment is the per-tenant rate limiter the
// POST /api/customdomains endpoint consults before recording a new
// custom-domain claim (SIN-62243 F45 deliverable 3).
//
// Quotas (all enforced together — exceeding any single one denies):
//
//	5  new domains / hour   per tenant
//	20 new domains / day    per tenant
//	50 new domains / month  per tenant
//	25 active (non-deleted) domains total per tenant — hard cap
//
// The hard cap is checked first because it is the cheapest (one count
// against a Postgres index). The rolling-window quotas hit Redis. The
// circuit-breaker is checked last so a tenant whose past issuances have
// failed is told "frozen" rather than "quota exceeded" (the dashboard
// reads the rejected reason to decide whether to page humans).
//
// The use-case stays HTTP-agnostic: callers (the POST handler in a
// future PR) translate Decision into 429 / 403 / 503.
package enrollment

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Decision encodes the outcome of an Allow call. Allowed means the
// caller may insert a new tenant_custom_domains row; the others all
// stand for "do not insert; report this reason back".
type Decision int

const (
	DecisionUnknown Decision = iota
	DecisionAllowed
	DecisionDeniedHardCap        // ≥25 active domains.
	DecisionDeniedHourlyQuota    // 5/hour exceeded.
	DecisionDeniedDailyQuota     // 20/day exceeded.
	DecisionDeniedMonthlyQuota   // 50/month exceeded.
	DecisionDeniedCircuitBreaker // tenant frozen by LE-failure breaker.
	DecisionError                // port failure; caller should 5xx.
)

func (d Decision) String() string {
	switch d {
	case DecisionAllowed:
		return "allowed"
	case DecisionDeniedHardCap:
		return "hard_cap"
	case DecisionDeniedHourlyQuota:
		return "hourly_quota"
	case DecisionDeniedDailyQuota:
		return "daily_quota"
	case DecisionDeniedMonthlyQuota:
		return "monthly_quota"
	case DecisionDeniedCircuitBreaker:
		return "circuit_breaker"
	case DecisionError:
		return "error"
	default:
		return "unknown"
	}
}

// Result is what Allow returns. Decision drives the wire response;
// QuotaWindow names which window (if any) the caller is over so the
// HTTP layer can tell the client when to retry. ResetAfter is best-
// effort, not a hard guarantee.
type Result struct {
	Decision   Decision
	ResetAfter time.Duration
	Err        error
}

// Quota holds the four rolling-window thresholds. Construct via
// DefaultQuota or override per-tenant for premium plans.
type Quota struct {
	Hourly  int
	Daily   int
	Monthly int
	HardCap int
}

// DefaultQuota returns the F45 spec defaults (5/h, 20/d, 50/mo, 25 active).
func DefaultQuota() Quota {
	return Quota{Hourly: 5, Daily: 20, Monthly: 50, HardCap: 25}
}

// CountStore returns the current count of active (non-deleted) domains
// per tenant. The Postgres adapter is the production implementation.
type CountStore interface {
	ActiveCount(ctx context.Context, tenantID uuid.UUID) (int, error)
}

// WindowCounter returns the count of "new domains created" for a tenant
// within the given window. The Redis adapter is the production
// implementation; rolling-window state lives in Redis sorted sets keyed
// by tenant + window-bucket.
type WindowCounter interface {
	// CountAndRecord increments the per-window counter for tenantID and
	// returns the post-increment count. The implementation MUST be
	// atomic so concurrent enrollments do not race the threshold.
	CountAndRecord(ctx context.Context, tenantID uuid.UUID, window Window, now time.Time) (int, error)
}

// Window names a rolling-window bucket.
type Window int

const (
	WindowHour Window = iota
	WindowDay
	WindowMonth
)

// Duration returns the fixed length of the window.
func (w Window) Duration() time.Duration {
	switch w {
	case WindowHour:
		return time.Hour
	case WindowDay:
		return 24 * time.Hour
	case WindowMonth:
		return 30 * 24 * time.Hour
	default:
		return time.Hour
	}
}

func (w Window) String() string {
	switch w {
	case WindowHour:
		return "hour"
	case WindowDay:
		return "day"
	case WindowMonth:
		return "month"
	default:
		return "unknown"
	}
}

// Breaker reports whether a tenant is currently frozen by the
// circuit-breaker. nil-Breaker means "no breaker wired"; the use-case
// then skips the check.
type Breaker interface {
	IsOpen(ctx context.Context, tenantID uuid.UUID, now time.Time) (bool, error)
}

// AuditLogger emits one structured event per Allow call for the
// audit_log timeline. The HTTP handler can attach request-scoped
// fields by wrapping; the use-case just calls Log with the bare facts.
type AuditLogger interface {
	LogEnrollmentDecision(ctx context.Context, tenantID uuid.UUID, decision Decision, reason string)
}

// Clock returns the current wall-clock time. Injected so tests can
// advance time deterministically.
type Clock func() time.Time

// UseCase is the enrollment quota gate. Construct once and reuse;
// safe for concurrent use as long as every embedded port is.
type UseCase struct {
	store   CountStore
	counter WindowCounter
	breaker Breaker
	audit   AuditLogger
	now     Clock
	quota   Quota
}

// New wires the use-case. breaker and audit MAY be nil (the gate is
// still functional; nil-breaker = always closed; nil-audit = silent).
func New(store CountStore, counter WindowCounter, breaker Breaker, audit AuditLogger, now Clock, quota Quota) *UseCase {
	if now == nil {
		now = time.Now
	}
	if quota.Hourly <= 0 || quota.Daily <= 0 || quota.Monthly <= 0 || quota.HardCap <= 0 {
		quota = DefaultQuota()
	}
	return &UseCase{
		store:   store,
		counter: counter,
		breaker: breaker,
		audit:   audit,
		now:     now,
		quota:   quota,
	}
}

// ErrZeroTenant is returned when tenantID is uuid.Nil. Caller bug.
var ErrZeroTenant = errors.New("enrollment: tenantID must not be uuid.Nil")

// Allow runs the full deny-by-default pipeline. Order:
//
//  1. Hard cap (cheap).
//  2. Hourly quota.
//  3. Daily quota.
//  4. Monthly quota.
//  5. Circuit breaker.
//
// The first denying check returns its Decision. Allowed runs all five
// and only succeeds when none deny.
func (u *UseCase) Allow(ctx context.Context, tenantID uuid.UUID) Result {
	if tenantID == uuid.Nil {
		return Result{Decision: DecisionError, Err: ErrZeroTenant}
	}
	now := u.now()

	active, err := u.store.ActiveCount(ctx, tenantID)
	if err != nil {
		u.logDecision(ctx, tenantID, DecisionError, "active_count_error")
		return Result{Decision: DecisionError, Err: err}
	}
	if active >= u.quota.HardCap {
		u.logDecision(ctx, tenantID, DecisionDeniedHardCap, "hard_cap")
		return Result{Decision: DecisionDeniedHardCap}
	}

	// After hard-cap passes, count-and-record across the three rolling
	// windows. We record speculatively and accept that a denied call
	// still leaves a +1 in lower windows — Redis does not support
	// transactional rollback across commands, so the rolling counters
	// over-count by ≤1 per denied call. The gap decays through window
	// expiry and the F45 spec accepts it.
	count, err := u.counter.CountAndRecord(ctx, tenantID, WindowHour, now)
	if err != nil {
		u.logDecision(ctx, tenantID, DecisionError, "hour_counter_error")
		return Result{Decision: DecisionError, Err: err}
	}
	if count > u.quota.Hourly {
		u.logDecision(ctx, tenantID, DecisionDeniedHourlyQuota, "hourly_quota")
		return Result{Decision: DecisionDeniedHourlyQuota, ResetAfter: WindowHour.Duration()}
	}

	count, err = u.counter.CountAndRecord(ctx, tenantID, WindowDay, now)
	if err != nil {
		u.logDecision(ctx, tenantID, DecisionError, "day_counter_error")
		return Result{Decision: DecisionError, Err: err}
	}
	if count > u.quota.Daily {
		u.logDecision(ctx, tenantID, DecisionDeniedDailyQuota, "daily_quota")
		return Result{Decision: DecisionDeniedDailyQuota, ResetAfter: WindowDay.Duration()}
	}

	count, err = u.counter.CountAndRecord(ctx, tenantID, WindowMonth, now)
	if err != nil {
		u.logDecision(ctx, tenantID, DecisionError, "month_counter_error")
		return Result{Decision: DecisionError, Err: err}
	}
	if count > u.quota.Monthly {
		u.logDecision(ctx, tenantID, DecisionDeniedMonthlyQuota, "monthly_quota")
		return Result{Decision: DecisionDeniedMonthlyQuota, ResetAfter: WindowMonth.Duration()}
	}

	if u.breaker != nil {
		open, err := u.breaker.IsOpen(ctx, tenantID, now)
		if err != nil {
			u.logDecision(ctx, tenantID, DecisionError, "breaker_error")
			return Result{Decision: DecisionError, Err: err}
		}
		if open {
			u.logDecision(ctx, tenantID, DecisionDeniedCircuitBreaker, "circuit_breaker")
			return Result{Decision: DecisionDeniedCircuitBreaker}
		}
	}

	u.logDecision(ctx, tenantID, DecisionAllowed, "allowed")
	return Result{Decision: DecisionAllowed}
}

func (u *UseCase) logDecision(ctx context.Context, tenantID uuid.UUID, d Decision, reason string) {
	if u.audit != nil {
		u.audit.LogEnrollmentDecision(ctx, tenantID, d, reason)
	}
}
