package tls_ask

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Clock returns the current wall-clock time. Injected so tests can advance
// time deterministically.
type Clock func() time.Time

// UseCase is the Ask decision pipeline. Construct one at startup and reuse;
// it is safe for concurrent use as long as every embedded port is.
type UseCase struct {
	repo  Repository
	rate  RateLimiter
	flag  FeatureFlag
	log   Logger
	now   Clock
	maxLen int
}

// New builds a UseCase. now defaults to time.Now when nil — kept overridable
// to keep the pipeline deterministic in unit tests. The host length cap of
// 253 bytes is the IDN-decoded DNS limit (RFC 1035 §2.3.4); inputs above it
// are denied as ReasonInvalidHost without a port call.
func New(repo Repository, rate RateLimiter, flag FeatureFlag, log Logger, now Clock) *UseCase {
	if now == nil {
		now = time.Now
	}
	return &UseCase{
		repo:   repo,
		rate:   rate,
		flag:   flag,
		log:    log,
		now:    now,
		maxLen: 253,
	}
}

// Ask runs the decision pipeline for one host. Order of checks is fixed:
//
//  1. Validate input syntactically (cheap, no I/O).
//  2. Consult the feature flag (cheap, in-memory or env).
//  3. Consult the rate limiter (Redis SETEX/ZADD).
//  4. Consult the repository (Postgres SELECT).
//
// Step ordering matters for the cost model: an attacker spamming a single
// host hits the rate limiter on the 4th call/minute and never reaches the
// DB. An attacker spamming many random hosts hits the DB but each query is
// indexed (LOWER(host) partial unique).
func (u *UseCase) Ask(ctx context.Context, rawHost string) Result {
	host := normalizeHost(rawHost)
	if host == "" || len(host) > u.maxLen {
		u.log.LogDeny(ctx, rawHost, ReasonInvalidHost)
		return Result{Decision: DecisionDeny, Reason: ReasonInvalidHost, Host: rawHost}
	}

	enabled, err := u.flag.AskEnabled(ctx)
	if err != nil {
		u.log.LogError(ctx, host, ReasonFeatureFlagError, err)
		return Result{Decision: DecisionError, Reason: ReasonFeatureFlagError, Host: host, Err: err}
	}
	if !enabled {
		u.log.LogDeny(ctx, host, ReasonDisabled)
		return Result{Decision: DecisionDisabled, Reason: ReasonDisabled, Host: host}
	}

	allowed, err := u.rate.Allow(ctx, host, u.now())
	if err != nil {
		u.log.LogError(ctx, host, ReasonRateLimitError, err)
		return Result{Decision: DecisionError, Reason: ReasonRateLimitError, Host: host, Err: err}
	}
	if !allowed {
		// rate-limited results do not get the deny structured log; the
		// dashboard distinguishes "denied because policy" from "denied
		// because flooding" via decision, not reason.
		return Result{Decision: DecisionRateLimited, Host: host}
	}

	rec, err := u.repo.Lookup(ctx, host)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			u.log.LogDeny(ctx, host, ReasonNotFound)
			return Result{Decision: DecisionDeny, Reason: ReasonNotFound, Host: host}
		}
		u.log.LogError(ctx, host, ReasonRepositoryError, err)
		return Result{Decision: DecisionError, Reason: ReasonRepositoryError, Host: host, Err: err}
	}

	if rec.VerifiedAt == nil {
		u.log.LogDeny(ctx, host, ReasonNotVerified)
		return Result{Decision: DecisionDeny, Reason: ReasonNotVerified, Host: host}
	}
	if rec.TLSPausedAt != nil {
		u.log.LogDeny(ctx, host, ReasonPaused)
		return Result{Decision: DecisionDeny, Reason: ReasonPaused, Host: host}
	}

	u.log.LogAllow(ctx, host)
	return Result{Decision: DecisionAllow, Host: host}
}

// normalizeHost lowercases and trims the host. Caddy passes hosts with the
// case the SNI client used; on-demand lookups must compare case-insensitively
// because DNS is. Returns "" if the host is syntactically invalid as a public
// DNS name — at minimum two labels, LDH characters per label, no leading or
// trailing hyphen on any label.
func normalizeHost(raw string) string {
	host := strings.TrimSpace(raw)
	host = strings.TrimSuffix(host, ".") // FQDN trailing dot
	host = strings.ToLower(host)
	if host == "" {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		// public host names always have at least two labels (TLD + name).
		return ""
	}
	for _, label := range labels {
		if label == "" {
			return "" // rejects ".." and leading/trailing dot
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= '0' && c <= '9':
			case c == '-' || c == '_':
			default:
				return ""
			}
		}
	}
	return host
}
