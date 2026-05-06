// Package circuitbreaker is the Let's Encrypt issuance circuit breaker
// (SIN-62243 F45 deliverable 4). It guards a tenant's ability to enroll
// new custom domains: if their recent issuances are failing the breaker
// trips, freezes new enrollments for 24h, and alerts ops on Slack.
//
// Threshold: 5 failures within 1h trips the breaker. The freeze lasts
// 24h from the trip event. Successful issuances reset the failure
// window.
//
// The use-case is hexagonal: persistence (failure log + freeze state)
// lives behind the State port; alerting lives behind the Alerter port.
// The Redis state adapter is the production wiring; tests use an
// in-memory fake.
package circuitbreaker

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Config tunes the breaker. Construct via DefaultConfig or override.
type Config struct {
	// Threshold is the number of failures within Window that trip the
	// breaker. F45 default: 5.
	Threshold int
	// Window is the rolling window for the failure count. F45 default:
	// 1 hour.
	Window time.Duration
	// FreezeFor is how long the breaker stays tripped after a trip
	// event. F45 default: 24 hours.
	FreezeFor time.Duration
}

// DefaultConfig returns the F45 spec defaults (5 failures / 1h → 24h freeze).
func DefaultConfig() Config {
	return Config{Threshold: 5, Window: time.Hour, FreezeFor: 24 * time.Hour}
}

// State captures the persistent breaker state per tenant.
//
// Concurrency contract:
//   - RecordFailure MUST be atomic with respect to itself: two
//     concurrent failures both observe the post-increment count, never
//     a stale snapshot.
//   - Trip MUST set frozen_until = now + freezeFor in a single op so a
//     reader cannot observe a half-tripped state.
type State interface {
	// RecordFailure adds (now, host) to the tenant's failure log,
	// drops entries older than (now - window), and returns the
	// current in-window failure count.
	RecordFailure(ctx context.Context, tenantID uuid.UUID, host string, now time.Time, window time.Duration) (int, error)
	// Trip marks the tenant frozen until now + freezeFor. Idempotent.
	Trip(ctx context.Context, tenantID uuid.UUID, now time.Time, freezeFor time.Duration) error
	// IsOpen returns true while the tenant is in the freeze window.
	IsOpen(ctx context.Context, tenantID uuid.UUID, now time.Time) (bool, error)
	// Reset clears any open trip for the tenant. Called on a
	// successful issuance to acknowledge that LE is healthy again.
	// Idempotent.
	Reset(ctx context.Context, tenantID uuid.UUID) error
}

// Alerter posts to the operations alert channel (Slack #alerts in
// production). Implementations MUST NOT block on transient failures —
// the breaker's alert is a side-effect, not the primary trip
// mechanism, so it should not stall the calling goroutine on retries.
type Alerter interface {
	AlertCircuitTripped(ctx context.Context, tenantID uuid.UUID, host string, failures int)
}

// Clock returns the current wall-clock time.
type Clock func() time.Time

// UseCase is the public API.
type UseCase struct {
	state   State
	alerter Alerter
	now     Clock
	cfg     Config
}

// New wires the breaker. alerter MAY be nil (alerts disabled).
func New(state State, alerter Alerter, now Clock, cfg Config) *UseCase {
	if now == nil {
		now = time.Now
	}
	if cfg.Threshold <= 0 || cfg.Window <= 0 || cfg.FreezeFor <= 0 {
		cfg = DefaultConfig()
	}
	return &UseCase{state: state, alerter: alerter, now: now, cfg: cfg}
}

// ErrZeroTenant is returned when tenantID is uuid.Nil.
var ErrZeroTenant = errors.New("circuitbreaker: tenantID must not be uuid.Nil")

// RecordFailure logs an LE issuance failure. If the post-increment
// count meets the threshold, the breaker trips and an alert is fired.
// Returns whether the breaker tripped on THIS call (false on
// subsequent calls within the same freeze window).
func (u *UseCase) RecordFailure(ctx context.Context, tenantID uuid.UUID, host string) (bool, error) {
	if tenantID == uuid.Nil {
		return false, ErrZeroTenant
	}
	now := u.now()
	count, err := u.state.RecordFailure(ctx, tenantID, host, now, u.cfg.Window)
	if err != nil {
		return false, err
	}
	if count < u.cfg.Threshold {
		return false, nil
	}
	if err := u.state.Trip(ctx, tenantID, now, u.cfg.FreezeFor); err != nil {
		return false, err
	}
	if u.alerter != nil {
		u.alerter.AlertCircuitTripped(ctx, tenantID, host, count)
	}
	return true, nil
}

// IsOpen reports whether the tenant is currently frozen. Read path used
// by enrollment.UseCase.
func (u *UseCase) IsOpen(ctx context.Context, tenantID uuid.UUID, now time.Time) (bool, error) {
	if tenantID == uuid.Nil {
		return false, ErrZeroTenant
	}
	return u.state.IsOpen(ctx, tenantID, now)
}

// RecordSuccess clears any open trip on a successful issuance. Idempotent.
func (u *UseCase) RecordSuccess(ctx context.Context, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return ErrZeroTenant
	}
	return u.state.Reset(ctx, tenantID)
}
