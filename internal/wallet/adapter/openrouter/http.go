package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/pericles-luz/crm/internal/wallet/port"
)

// Default HTTP adapter wiring. The cost endpoint version is pinned in
// docs/adr/0001-openrouter-cost-adapter.md; bumping it here requires a
// new ADR revision.
const (
	DefaultBaseURL    = "https://openrouter.ai"
	DailyUsagePath    = "/api/v1/credits/daily"
	DefaultTimeout    = 30 * time.Second
	DefaultMaxRetries = 2
	maxResponseBytes  = 1 << 20 // 1 MiB cap so a misbehaving upstream cannot OOM us
)

// ErrAuth is the sentinel for 401/403 responses from OpenRouter; rotate
// the API key when this surfaces.
var ErrAuth = errors.New("openrouter: authentication failed")

// ErrRateLimit is the sentinel matched by errors.Is for any 429
// response. The concrete error is *RateLimitError carrying RetryAfter.
var ErrRateLimit = errors.New("openrouter: rate limited")

// RateLimitError is returned for 429 responses. RetryAfter is the
// parsed value of the Retry-After header; zero means upstream did not
// provide one.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("openrouter: rate limited; retry after %s", e.RetryAfter)
	}
	return "openrouter: rate limited"
}

// Is lets errors.Is(err, ErrRateLimit) succeed for RateLimitError.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimit }

// Client is the production OpenRouter cost-API adapter. It implements
// port.OpenRouterCostAPI and is constructed once per process with the
// API key taken from OPENROUTER_API_KEY by the cmd entrypoint.
type Client struct {
	baseURL    string
	httpClient *http.Client
	apiKey     string
	sleep      func(time.Duration)
	maxRetries int
}

// Option mutates the Client during construction.
type Option func(*Client)

// WithBaseURL overrides the OpenRouter host, mainly used by tests
// pointing at httptest.NewServer.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = u
		}
	}
}

// WithHTTPClient swaps the underlying HTTP client (e.g. to use a custom
// transport with mTLS or a shorter timeout).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// WithSleeper replaces time.Sleep so tests can assert Retry-After
// behaviour without real wall-clock waits.
func WithSleeper(s func(time.Duration)) Option {
	return func(c *Client) {
		if s != nil {
			c.sleep = s
		}
	}
}

// WithMaxRetries bounds how many times we retry on 429 / 5xx. Negative
// values are clamped to 0.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n < 0 {
			n = 0
		}
		c.maxRetries = n
	}
}

// New returns a Client. The apiKey MUST come from OPENROUTER_API_KEY
// (or another secret store); the cmd/walletreconciler wiring enforces
// that contract.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: DefaultTimeout},
		apiKey:     apiKey,
		sleep:      time.Sleep,
		maxRetries: DefaultMaxRetries,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// DailyUsage fetches one UTC-day of OpenRouter usage for masterID.
// Implements port.OpenRouterCostAPI.
func (c *Client) DailyUsage(ctx context.Context, masterID string, day time.Time) (port.OpenRouterCostSample, error) {
	if masterID == "" {
		return port.OpenRouterCostSample{}, errors.New("openrouter: master_id required")
	}
	if c.apiKey == "" {
		return port.OpenRouterCostSample{}, fmt.Errorf("%w: api key not configured", ErrAuth)
	}

	dayUTC := day.UTC().Truncate(24 * time.Hour)
	q := url.Values{}
	q.Set("master_id", masterID)
	q.Set("date", dayUTC.Format("2006-01-02"))
	endpoint := c.baseURL + DailyUsagePath + "?" + q.Encode()

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		sample, retryable, err := c.doRequest(ctx, endpoint, masterID, dayUTC)
		if err == nil {
			return sample, nil
		}
		lastErr = err
		if !retryable || attempt == c.maxRetries {
			return port.OpenRouterCostSample{}, err
		}
		if waitErr := c.waitBeforeRetry(ctx, err); waitErr != nil {
			return port.OpenRouterCostSample{}, waitErr
		}
	}
	return port.OpenRouterCostSample{}, lastErr
}

// dailyUsageResponse mirrors the documented OpenRouter cost endpoint
// shape (see docs/adr/0001-openrouter-cost-adapter.md). We accept the
// `data` envelope and read total_tokens directly — the conversion
// factor between OpenRouter `cost_usd` and our wallet token unit is
// pinned at 1 token per OpenRouter-reported token (pass-through), so
// the adapter just propagates the integer.
type dailyUsageResponse struct {
	Data struct {
		MasterID    string  `json:"master_id"`
		Date        string  `json:"date"`
		TotalTokens int64   `json:"total_tokens"`
		CostUSD     float64 `json:"cost_usd"`
	} `json:"data"`
}

func (c *Client) doRequest(ctx context.Context, endpoint, masterID string, day time.Time) (port.OpenRouterCostSample, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return port.OpenRouterCostSample{}, false, fmt.Errorf("openrouter: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return port.OpenRouterCostSample{}, true, fmt.Errorf("openrouter: transport: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var body dailyUsageResponse
		dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
		if err := dec.Decode(&body); err != nil {
			return port.OpenRouterCostSample{}, false, fmt.Errorf("openrouter: decode: %w", err)
		}
		if body.Data.TotalTokens < 0 {
			return port.OpenRouterCostSample{}, false, fmt.Errorf("openrouter: negative total_tokens=%d", body.Data.TotalTokens)
		}
		return port.OpenRouterCostSample{
			MasterID: masterID,
			Date:     day,
			Tokens:   body.Data.TotalTokens,
		}, false, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return port.OpenRouterCostSample{}, false, fmt.Errorf("%w: status=%d", ErrAuth, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return port.OpenRouterCostSample{}, true, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return port.OpenRouterCostSample{}, true, fmt.Errorf("openrouter: server error %d", resp.StatusCode)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return port.OpenRouterCostSample{}, false, fmt.Errorf("openrouter: unexpected status %d: %s", resp.StatusCode, string(snippet))
	}
}

func (c *Client) waitBeforeRetry(ctx context.Context, err error) error {
	wait := 0 * time.Second
	var rle *RateLimitError
	if errors.As(err, &rle) {
		wait = rle.RetryAfter
	}
	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.sleep(wait)
	return nil
}

// parseRetryAfter accepts either a delta-seconds integer or an HTTP
// date per RFC 7231 §7.1.3. Returns zero on parse failure.
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
