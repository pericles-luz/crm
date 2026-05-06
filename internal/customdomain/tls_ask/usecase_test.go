package tls_ask_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

// fakeRepo is the in-memory Repository used by the use-case tests. The CTO
// quality bar forbids mocking the database when the code under test
// touches storage; this is the use-case layer, which is hexagonal — the
// Repository is a port that some test/integration layer satisfies.
type fakeRepo struct {
	records map[string]tls_ask.DomainRecord
	err     error
	calls   int
}

func (r *fakeRepo) Lookup(_ context.Context, host string) (tls_ask.DomainRecord, error) {
	r.calls++
	if r.err != nil {
		return tls_ask.DomainRecord{}, r.err
	}
	rec, ok := r.records[host]
	if !ok {
		return tls_ask.DomainRecord{}, tls_ask.ErrNotFound
	}
	return rec, nil
}

type fakeRate struct {
	mu      sync.Mutex
	allow   bool
	err     error
	hosts   []string
	atTimes []time.Time
}

func (r *fakeRate) Allow(_ context.Context, host string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts = append(r.hosts, host)
	r.atTimes = append(r.atTimes, now)
	if r.err != nil {
		return false, r.err
	}
	return r.allow, nil
}

type fakeFlag struct {
	enabled bool
	err     error
}

func (f *fakeFlag) AskEnabled(context.Context) (bool, error) {
	return f.enabled, f.err
}

type captureLogger struct {
	mu      sync.Mutex
	denies  []string
	allows  []string
	errors  []string
	reasons []tls_ask.Reason
}

func (l *captureLogger) LogDeny(_ context.Context, host string, reason tls_ask.Reason) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.denies = append(l.denies, host)
	l.reasons = append(l.reasons, reason)
}

func (l *captureLogger) LogAllow(_ context.Context, host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allows = append(l.allows, host)
}

func (l *captureLogger) LogError(_ context.Context, host string, reason tls_ask.Reason, _ error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, host)
	l.reasons = append(l.reasons, reason)
}

// fixedNow returns a deterministic clock for the use-case tests.
func fixedNow(t time.Time) tls_ask.Clock {
	return func() time.Time { return t }
}

func TestAsk_AllowsVerifiedActiveDomain(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	repo := &fakeRepo{records: map[string]tls_ask.DomainRecord{
		"shop.example.com": {VerifiedAt: &verified},
	}}
	rate := &fakeRate{allow: true}
	flag := &fakeFlag{enabled: true}
	log := &captureLogger{}
	uc := tls_ask.New(repo, rate, flag, log, fixedNow(verified.Add(time.Hour)))

	got := uc.Ask(context.Background(), "shop.example.com")

	if got.Decision != tls_ask.DecisionAllow {
		t.Fatalf("decision = %v, want allow", got.Decision)
	}
	if got.Host != "shop.example.com" {
		t.Fatalf("host = %q, want shop.example.com", got.Host)
	}
	if len(log.allows) != 1 || log.allows[0] != "shop.example.com" {
		t.Fatalf("allow log not emitted: %+v", log.allows)
	}
	if len(log.denies) != 0 {
		t.Fatalf("deny log unexpected: %+v", log.denies)
	}
}

func TestAsk_DeniesUnknownHost(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{records: map[string]tls_ask.DomainRecord{}}
	uc := tls_ask.New(repo, &fakeRate{allow: true}, &fakeFlag{enabled: true}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "evil.example.org")

	if got.Decision != tls_ask.DecisionDeny || got.Reason != tls_ask.ReasonNotFound {
		t.Fatalf("decision/reason = %v/%v, want deny/not_found", got.Decision, got.Reason)
	}
}

func TestAsk_DeniesUnverifiedDomain(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{records: map[string]tls_ask.DomainRecord{
		"pending.example.com": {VerifiedAt: nil},
	}}
	uc := tls_ask.New(repo, &fakeRate{allow: true}, &fakeFlag{enabled: true}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "pending.example.com")

	if got.Decision != tls_ask.DecisionDeny || got.Reason != tls_ask.ReasonNotVerified {
		t.Fatalf("decision/reason = %v/%v, want deny/not_verified", got.Decision, got.Reason)
	}
}

func TestAsk_DeniesPausedDomain(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	paused := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	repo := &fakeRepo{records: map[string]tls_ask.DomainRecord{
		"frozen.example.com": {VerifiedAt: &verified, TLSPausedAt: &paused},
	}}
	uc := tls_ask.New(repo, &fakeRate{allow: true}, &fakeFlag{enabled: true}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "frozen.example.com")

	if got.Decision != tls_ask.DecisionDeny || got.Reason != tls_ask.ReasonPaused {
		t.Fatalf("decision/reason = %v/%v, want deny/paused", got.Decision, got.Reason)
	}
}

func TestAsk_RateLimited(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	repo := &fakeRepo{records: map[string]tls_ask.DomainRecord{
		"shop.example.com": {VerifiedAt: &verified},
	}}
	uc := tls_ask.New(repo, &fakeRate{allow: false}, &fakeFlag{enabled: true}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "shop.example.com")

	if got.Decision != tls_ask.DecisionRateLimited {
		t.Fatalf("decision = %v, want rate_limited", got.Decision)
	}
	if repo.calls != 0 {
		t.Fatalf("repo called %d times after rate limit; want 0", repo.calls)
	}
}

func TestAsk_FeatureFlagOff(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{records: map[string]tls_ask.DomainRecord{}}
	rate := &fakeRate{allow: true}
	uc := tls_ask.New(repo, rate, &fakeFlag{enabled: false}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "shop.example.com")

	if got.Decision != tls_ask.DecisionDisabled || got.Reason != tls_ask.ReasonDisabled {
		t.Fatalf("decision/reason = %v/%v, want disabled/disabled", got.Decision, got.Reason)
	}
	if len(rate.hosts) != 0 {
		t.Fatalf("rate limiter consulted while flag off: %+v", rate.hosts)
	}
}

func TestAsk_RepositoryErrorIsTransient(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection refused")
	repo := &fakeRepo{err: boom}
	uc := tls_ask.New(repo, &fakeRate{allow: true}, &fakeFlag{enabled: true}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "shop.example.com")

	if got.Decision != tls_ask.DecisionError || got.Reason != tls_ask.ReasonRepositoryError {
		t.Fatalf("decision/reason = %v/%v, want error/repository_error", got.Decision, got.Reason)
	}
	if !errors.Is(got.Err, boom) {
		t.Fatalf("err = %v, want wrap of %v", got.Err, boom)
	}
}

func TestAsk_RateLimiterErrorFailsClosed(t *testing.T) {
	t.Parallel()
	boom := errors.New("redis: timeout")
	uc := tls_ask.New(&fakeRepo{}, &fakeRate{err: boom}, &fakeFlag{enabled: true}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "shop.example.com")

	if got.Decision != tls_ask.DecisionError || got.Reason != tls_ask.ReasonRateLimitError {
		t.Fatalf("decision/reason = %v/%v, want error/rate_limit_error", got.Decision, got.Reason)
	}
}

func TestAsk_FeatureFlagErrorFailsClosed(t *testing.T) {
	t.Parallel()
	boom := errors.New("flag store: timeout")
	uc := tls_ask.New(&fakeRepo{}, &fakeRate{allow: true}, &fakeFlag{err: boom}, &captureLogger{}, nil)

	got := uc.Ask(context.Background(), "shop.example.com")

	if got.Decision != tls_ask.DecisionError || got.Reason != tls_ask.ReasonFeatureFlagError {
		t.Fatalf("decision/reason = %v/%v, want error/feature_flag_error", got.Decision, got.Reason)
	}
}

func TestAsk_InvalidHostIsRejectedAtBoundary(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		" ",
		"with space.com",
		".leading-dot.com",
		"trailing-dot.",
		"-bad.example.com",
		"bad-.example.com",
		"double..dot.com",
		"weird/path.com",
		strings.Repeat("a.", 200),
	}
	for _, in := range cases {
		in := in
		t.Run("invalid:"+in, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepo{}
			rate := &fakeRate{allow: true}
			uc := tls_ask.New(repo, rate, &fakeFlag{enabled: true}, &captureLogger{}, nil)
			got := uc.Ask(context.Background(), in)
			if got.Decision != tls_ask.DecisionDeny || got.Reason != tls_ask.ReasonInvalidHost {
				t.Fatalf("decision/reason = %v/%v for %q, want deny/invalid_host", got.Decision, got.Reason, in)
			}
			if repo.calls != 0 {
				t.Fatalf("repo called for invalid host %q", in)
			}
			if len(rate.hosts) != 0 {
				t.Fatalf("rate limiter called for invalid host %q", in)
			}
		})
	}
}

func TestAsk_HostNormalization(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	repo := &fakeRepo{records: map[string]tls_ask.DomainRecord{
		"shop.example.com": {VerifiedAt: &verified},
	}}
	rate := &fakeRate{allow: true}
	uc := tls_ask.New(repo, rate, &fakeFlag{enabled: true}, &captureLogger{}, nil)

	for _, in := range []string{"Shop.Example.COM", "shop.example.com.", "  shop.example.com  "} {
		got := uc.Ask(context.Background(), in)
		if got.Decision != tls_ask.DecisionAllow {
			t.Fatalf("decision = %v for %q, want allow", got.Decision, in)
		}
	}
	if rate.hosts[0] != "shop.example.com" {
		t.Fatalf("rate limiter received un-normalized host: %q", rate.hosts[0])
	}
}

func TestAsk_DecisionAndReasonStrings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d tls_ask.Decision
		s string
	}{
		{tls_ask.DecisionAllow, "allow"},
		{tls_ask.DecisionDeny, "deny"},
		{tls_ask.DecisionRateLimited, "rate_limited"},
		{tls_ask.DecisionDisabled, "disabled"},
		{tls_ask.DecisionError, "error"},
		{tls_ask.DecisionUnknown, "unknown"},
	}
	for _, c := range cases {
		if got := c.d.String(); got != c.s {
			t.Errorf("Decision(%d).String() = %q, want %q", c.d, got, c.s)
		}
	}
	rcases := []struct {
		r tls_ask.Reason
		s string
	}{
		{tls_ask.ReasonNotFound, "not_found"},
		{tls_ask.ReasonNotVerified, "not_verified"},
		{tls_ask.ReasonPaused, "paused"},
		{tls_ask.ReasonInvalidHost, "invalid_host"},
		{tls_ask.ReasonRepositoryError, "repository_error"},
		{tls_ask.ReasonRateLimitError, "rate_limit_error"},
		{tls_ask.ReasonFeatureFlagError, "feature_flag_error"},
		{tls_ask.ReasonDisabled, "disabled"},
		{tls_ask.ReasonNone, ""},
	}
	for _, c := range rcases {
		if got := c.r.String(); got != c.s {
			t.Errorf("Reason(%d).String() = %q, want %q", c.r, got, c.s)
		}
	}
}
