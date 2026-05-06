package tlsask_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/transport/http/tlsask"
	"github.com/pericles-luz/crm/internal/customdomain/tls_ask"
)

type stubRepo struct {
	rec tls_ask.DomainRecord
	err error
}

func (s *stubRepo) Lookup(_ context.Context, _ string) (tls_ask.DomainRecord, error) {
	if s.err != nil {
		return tls_ask.DomainRecord{}, s.err
	}
	return s.rec, nil
}

type stubRate struct {
	allow bool
	err   error
}

func (s *stubRate) Allow(context.Context, string, time.Time) (bool, error) {
	return s.allow, s.err
}

type stubFlag struct {
	enabled bool
	err     error
}

func (s *stubFlag) AskEnabled(context.Context) (bool, error) {
	return s.enabled, s.err
}

// silentLogger ignores every log call. The handler tests assert on
// HTTP shape, not log emissions; the slog adapter has its own coverage.
type silentLogger struct{}

func (l *silentLogger) LogAllow(context.Context, string)                        {}
func (l *silentLogger) LogDeny(context.Context, string, tls_ask.Reason)         {}
func (l *silentLogger) LogError(context.Context, string, tls_ask.Reason, error) {}

func newHandler(repo tls_ask.Repository, rate tls_ask.RateLimiter, flag tls_ask.FeatureFlag) http.Handler {
	uc := tls_ask.New(repo, rate, flag, &silentLogger{}, nil)
	return tlsask.New(uc)
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if w.Body.Len() == 0 {
		return out
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return out
}

func TestHandler_AllowReturns200(t *testing.T) {
	t.Parallel()
	v := time.Now()
	h := newHandler(
		&stubRepo{rec: tls_ask.DomainRecord{VerifiedAt: &v}},
		&stubRate{allow: true},
		&stubFlag{enabled: true},
	)
	req := httptest.NewRequest(http.MethodGet, tlsask.Path+"?domain=shop.example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := decode(t, w)
	if body["status"] != "allow" {
		t.Fatalf("status field = %v", body["status"])
	}
}

func TestHandler_UnknownHost403(t *testing.T) {
	t.Parallel()
	h := newHandler(
		&stubRepo{err: tls_ask.ErrNotFound},
		&stubRate{allow: true},
		&stubFlag{enabled: true},
	)
	req := httptest.NewRequest(http.MethodGet, tlsask.Path+"?domain=evil.example.org", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	body := decode(t, w)
	if body["reason"] != "not_found" {
		t.Fatalf("reason = %v, want not_found", body["reason"])
	}
}

func TestHandler_NotVerified403(t *testing.T) {
	t.Parallel()
	h := newHandler(
		&stubRepo{rec: tls_ask.DomainRecord{}},
		&stubRate{allow: true},
		&stubFlag{enabled: true},
	)
	req := httptest.NewRequest(http.MethodGet, tlsask.Path+"?domain=pending.example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if reason := decode(t, w)["reason"]; reason != "not_verified" {
		t.Fatalf("reason = %v", reason)
	}
}

func TestHandler_Paused403(t *testing.T) {
	t.Parallel()
	v := time.Now()
	p := time.Now()
	h := newHandler(
		&stubRepo{rec: tls_ask.DomainRecord{VerifiedAt: &v, TLSPausedAt: &p}},
		&stubRate{allow: true},
		&stubFlag{enabled: true},
	)
	req := httptest.NewRequest(http.MethodGet, tlsask.Path+"?domain=frozen.example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if reason := decode(t, w)["reason"]; reason != "paused" {
		t.Fatalf("reason = %v", reason)
	}
}

func TestHandler_RateLimited429WithRetryAfter(t *testing.T) {
	t.Parallel()
	h := newHandler(
		&stubRepo{},
		&stubRate{allow: false},
		&stubFlag{enabled: true},
	)
	req := httptest.NewRequest(http.MethodGet, tlsask.Path+"?domain=shop.example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
}

func TestHandler_FlagOff503(t *testing.T) {
	t.Parallel()
	h := newHandler(&stubRepo{}, &stubRate{allow: true}, &stubFlag{enabled: false})
	req := httptest.NewRequest(http.MethodGet, tlsask.Path+"?domain=shop.example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	body := decode(t, w)
	if body["reason"] != "disabled" {
		t.Fatalf("reason = %v, want disabled", body["reason"])
	}
	if body["message"] == "" {
		t.Fatalf("message empty; expected human-readable hint")
	}
}

func TestHandler_RepositoryError500(t *testing.T) {
	t.Parallel()
	h := newHandler(
		&stubRepo{err: errors.New("connection refused")},
		&stubRate{allow: true},
		&stubFlag{enabled: true},
	)
	req := httptest.NewRequest(http.MethodGet, tlsask.Path+"?domain=shop.example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestHandler_MethodNotAllowed405(t *testing.T) {
	t.Parallel()
	h := newHandler(&stubRepo{}, &stubRate{allow: true}, &stubFlag{enabled: true})
	req := httptest.NewRequest(http.MethodPost, tlsask.Path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow header = %q, want GET, HEAD", got)
	}
}

func TestHandler_MissingDomainParam403(t *testing.T) {
	t.Parallel()
	h := newHandler(&stubRepo{}, &stubRate{allow: true}, &stubFlag{enabled: true})
	req := httptest.NewRequest(http.MethodGet, tlsask.Path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if reason := decode(t, w)["reason"]; reason != "invalid_host" {
		t.Fatalf("reason = %v, want invalid_host", reason)
	}
}

func TestHandler_HEADWorks(t *testing.T) {
	t.Parallel()
	v := time.Now()
	h := newHandler(
		&stubRepo{rec: tls_ask.DomainRecord{VerifiedAt: &v}},
		&stubRate{allow: true},
		&stubFlag{enabled: true},
	)
	req := httptest.NewRequest(http.MethodHead, tlsask.Path+"?domain=shop.example.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}
