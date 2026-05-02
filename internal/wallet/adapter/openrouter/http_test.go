package openrouter_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/wallet/adapter/openrouter"
)

// recordedResponse is the shape we receive from the production cost
// endpoint. The fixture below was captured from the documented
// /api/v1/credits/daily contract pinned in
// docs/adr/0001-openrouter-cost-adapter.md.
const recordedResponse = `{
  "data": {
    "master_id": "master-abc",
    "date": "2026-05-01",
    "total_tokens": 12345,
    "cost_usd": 1.234567
  }
}`

func TestClient_DailyUsage_HappyPath(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s, want GET", r.Method)
		}
		if r.URL.Path != openrouter.DailyUsagePath {
			t.Errorf("path: got %s, want %s", r.URL.Path, openrouter.DailyUsagePath)
		}
		if got := r.URL.Query().Get("master_id"); got != "master-abc" {
			t.Errorf("master_id query: got %q, want master-abc", got)
		}
		if got := r.URL.Query().Get("date"); got != "2026-05-01" {
			t.Errorf("date query: got %q, want 2026-05-01", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("auth header: got %q, want Bearer secret-key", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept header: got %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recordedResponse))
	}))
	defer srv.Close()

	c := openrouter.New("secret-key", openrouter.WithBaseURL(srv.URL))
	got, err := c.DailyUsage(context.Background(), "master-abc", day)
	if err != nil {
		t.Fatalf("DailyUsage: %v", err)
	}
	if got.MasterID != "master-abc" {
		t.Errorf("MasterID: got %q, want master-abc", got.MasterID)
	}
	if got.Tokens != 12345 {
		t.Errorf("Tokens: got %d, want 12345", got.Tokens)
	}
	if !got.Date.Equal(day) {
		t.Errorf("Date: got %v, want %v", got.Date, day)
	}
}

// TestClient_DailyUsage_TruncatesDay confirms a non-UTC, mid-day input
// is normalised to the start of the UTC day before the request fires.
func TestClient_DailyUsage_TruncatesDay(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	day := time.Date(2026, 5, 1, 18, 30, 0, 0, loc) // 21:30 UTC

	var observedDate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedDate = r.URL.Query().Get("date")
		_, _ = w.Write([]byte(recordedResponse))
	}))
	defer srv.Close()

	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	if _, err := c.DailyUsage(context.Background(), "m", day); err != nil {
		t.Fatalf("DailyUsage: %v", err)
	}
	if observedDate != "2026-05-01" {
		t.Errorf("date query: got %q, want 2026-05-01 (UTC)", observedDate)
	}
}

func TestClient_DailyUsage_AuthFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			c := openrouter.New("k", openrouter.WithBaseURL(srv.URL), openrouter.WithMaxRetries(0))
			_, err := c.DailyUsage(context.Background(), "m", time.Now())
			if !errors.Is(err, openrouter.ErrAuth) {
				t.Fatalf("err: got %v, want errors.Is ErrAuth", err)
			}
		})
	}
}

func TestClient_DailyUsage_MissingAPIKey(t *testing.T) {
	t.Parallel()
	c := openrouter.New("")
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if !errors.Is(err, openrouter.ErrAuth) {
		t.Fatalf("missing key err: got %v, want ErrAuth", err)
	}
}

func TestClient_DailyUsage_MissingMasterID(t *testing.T) {
	t.Parallel()
	c := openrouter.New("k")
	_, err := c.DailyUsage(context.Background(), "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "master_id") {
		t.Fatalf("missing master_id err: got %v", err)
	}
}

// TestClient_DailyUsage_RateLimitedRetriesAndSucceeds covers the
// Retry-After path: first response is 429 with Retry-After: 2, second
// is 200. We capture the sleep call to assert the adapter waited.
func TestClient_DailyUsage_RateLimitedRetriesAndSucceeds(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(recordedResponse))
	}))
	defer srv.Close()

	var slept time.Duration
	c := openrouter.New("k",
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithMaxRetries(1),
		openrouter.WithSleeper(func(d time.Duration) { slept = d }),
	)
	got, err := c.DailyUsage(context.Background(), "m", time.Now())
	if err != nil {
		t.Fatalf("DailyUsage: %v", err)
	}
	if got.Tokens != 12345 {
		t.Fatalf("Tokens: got %d", got.Tokens)
	}
	if slept != 2*time.Second {
		t.Errorf("sleep: got %s, want 2s", slept)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls: got %d, want 2", calls)
	}
}

// TestClient_DailyUsage_RateLimitedExhausted confirms that hitting
// maxRetries on 429 surfaces ErrRateLimit to the caller (so the cron
// can drop the alert and let the next pass try again).
func TestClient_DailyUsage_RateLimitedExhausted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := openrouter.New("k",
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithMaxRetries(1),
		openrouter.WithSleeper(func(time.Duration) {}),
	)
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if !errors.Is(err, openrouter.ErrRateLimit) {
		t.Fatalf("err: got %v, want errors.Is ErrRateLimit", err)
	}
	var rle *openrouter.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err: cannot unwrap to *RateLimitError; got %T", err)
	}
}

func TestClient_DailyUsage_ServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := openrouter.New("k",
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithMaxRetries(0),
	)
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if err == nil || !strings.Contains(err.Error(), "server error 500") {
		t.Fatalf("server-error: got %v, want server error 500", err)
	}
}

func TestClient_DailyUsage_UnexpectedStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if err == nil || !strings.Contains(err.Error(), "418") {
		t.Fatalf("unexpected-status: got %v", err)
	}
}

func TestClient_DailyUsage_DecodeError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{malformed"))
	}))
	defer srv.Close()
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("decode error: got %v", err)
	}
}

func TestClient_DailyUsage_NegativeTokens(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"total_tokens":-1}}`))
	}))
	defer srv.Close()
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative tokens: got %v", err)
	}
}

// TestClient_DailyUsage_TransportError forces a TCP-level failure by
// hitting a closed server.
func TestClient_DailyUsage_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c := openrouter.New("k",
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithMaxRetries(0),
		openrouter.WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}),
	)
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("transport error: got %v", err)
	}
}

// TestClient_DailyUsage_ContextCancelDuringRetry asserts a cancelled
// context aborts the retry-after wait.
func TestClient_DailyUsage_ContextCancelDuringRetry(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := openrouter.New("k",
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithMaxRetries(1),
		openrouter.WithSleeper(func(time.Duration) { t.Fatal("sleep should not be called once ctx is cancelled") }),
	)
	if _, err := c.DailyUsage(ctx, "m", time.Now()); err == nil {
		t.Fatalf("expected error from cancelled context")
	}
}

func TestClient_DailyUsage_BadBaseURL(t *testing.T) {
	t.Parallel()
	c := openrouter.New("k", openrouter.WithBaseURL("http://%%bad-url"))
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if err == nil {
		t.Fatalf("expected error for invalid base URL")
	}
}

func TestClient_DailyUsage_NegativeMaxRetriesClamped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(recordedResponse))
	}))
	defer srv.Close()
	c := openrouter.New("k",
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithMaxRetries(-5),
	)
	if _, err := c.DailyUsage(context.Background(), "m", time.Now()); err != nil {
		t.Fatalf("DailyUsage: %v", err)
	}
}

func TestRateLimitError_FormattingAndIs(t *testing.T) {
	t.Parallel()
	rle := &openrouter.RateLimitError{RetryAfter: 5 * time.Second}
	if !strings.Contains(rle.Error(), "5s") {
		t.Errorf("Error: got %q, want substring 5s", rle.Error())
	}
	empty := &openrouter.RateLimitError{}
	if strings.Contains(empty.Error(), "after") {
		t.Errorf("empty Error: got %q, want no 'after' clause", empty.Error())
	}
	if !errors.Is(rle, openrouter.ErrRateLimit) {
		t.Errorf("errors.Is: want true")
	}
	if errors.Is(rle, openrouter.ErrAuth) {
		t.Errorf("errors.Is(ErrAuth): want false")
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	t.Parallel()
	// Use an HTTP-date roughly 5 seconds in the future.
	future := time.Now().UTC().Add(5 * time.Second).Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", future)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var slept time.Duration
	c := openrouter.New("k",
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithMaxRetries(0),
		openrouter.WithSleeper(func(d time.Duration) { slept = d }),
	)
	_, err := c.DailyUsage(context.Background(), "m", time.Now())
	if !errors.Is(err, openrouter.ErrRateLimit) {
		t.Fatalf("err: got %v, want ErrRateLimit", err)
	}
	// The first attempt fails and we never retry (maxRetries=0), so
	// sleep is never called — but the inner RateLimitError must carry
	// a positive RetryAfter parsed from the HTTP date.
	if slept != 0 {
		t.Errorf("sleep: got %s, want 0 with maxRetries=0", slept)
	}
	var rle *openrouter.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("cannot unwrap to RateLimitError")
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > 30*time.Second {
		t.Errorf("RetryAfter: got %s, want 0<x<=30s", rle.RetryAfter)
	}
}

func TestParseRetryAfter_NegativeAndPast(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":      "",
		"negative":   "-3",
		"unparsable": "soon",
		"past-date":  time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat),
	}
	for name, header := range cases {
		header := header
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if header != "" {
					w.Header().Set("Retry-After", header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()
			c := openrouter.New("k",
				openrouter.WithBaseURL(srv.URL),
				openrouter.WithMaxRetries(0),
				openrouter.WithSleeper(func(time.Duration) {}),
			)
			_, err := c.DailyUsage(context.Background(), "m", time.Now())
			var rle *openrouter.RateLimitError
			if !errors.As(err, &rle) {
				t.Fatalf("not a RateLimitError: %v", err)
			}
			if rle.RetryAfter != 0 {
				t.Errorf("RetryAfter: got %s, want 0 for header %q", rle.RetryAfter, header)
			}
		})
	}
}

// TestClient_OptionsIgnoreInvalid asserts the adapter ignores zero/nil
// option overrides instead of overwriting defaults.
func TestClient_OptionsIgnoreInvalid(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(recordedResponse))
	}))
	defer srv.Close()
	c := openrouter.New("k",
		openrouter.WithBaseURL(""), // ignored
		openrouter.WithBaseURL(srv.URL),
		openrouter.WithHTTPClient(nil),         // ignored
		openrouter.WithSleeper(nil),            // ignored
		openrouter.WithSleeper(time.Sleep),     // explicit
		openrouter.WithHTTPClient(http.DefaultClient),
	)
	if _, err := c.DailyUsage(context.Background(), "m", time.Now()); err != nil {
		t.Fatalf("DailyUsage with override-then-explicit options: %v", err)
	}
}

