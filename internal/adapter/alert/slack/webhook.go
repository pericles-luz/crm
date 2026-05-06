// Package slack adapts the circuit-breaker Alerter port to a Slack
// incoming webhook (SIN-62243 F45 deliverable 4 — alerts on
// #alerts).
//
// The webhook URL is treated as a secret: this package never logs it
// (only the host) and refuses to construct a client with an empty URL.
// Posts use a 5s timeout so a Slack outage does not stall the calling
// goroutine; the alert is fire-and-forget and a missed post degrades to
// "ops finds out from the dashboard" rather than "issuance keeps
// happening".
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Webhook is the Slack adapter. Construct via New; safe for concurrent use.
type Webhook struct {
	url    string
	client *http.Client
}

// HTTPClient is the narrow http surface the adapter uses. Defining it
// here keeps tests free of the *http.Client type and lets us substitute
// a recording double.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// ErrEmptyURL is returned by New if the webhook URL is empty.
var ErrEmptyURL = errors.New("slack: webhook url is empty")

// New builds a Webhook bound to url. client may be nil — the default is
// a 5s-timeout *http.Client.
func New(url string, client HTTPClient) (*Webhook, error) {
	if url == "" {
		return nil, ErrEmptyURL
	}
	c := client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	httpC, ok := c.(*http.Client)
	if !ok {
		// Wrap a custom HTTPClient with a clientAdapter so we keep one
		// internal field type. Cleaner than handling two pointer types
		// at every call site.
		return &Webhook{url: url, client: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return c.Do(r)
		})}}, nil
	}
	return &Webhook{url: url, client: httpC}, nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// AlertCircuitTripped implements circuitbreaker.Alerter. The post is
// fire-and-forget: errors are surfaced via the returned error of an
// internal Post call but the Alerter contract is void. Callers that
// want stricter delivery semantics should wrap the adapter.
func (w *Webhook) AlertCircuitTripped(ctx context.Context, tenantID uuid.UUID, host string, failures int) {
	body := map[string]string{
		"text": fmt.Sprintf(
			":rotating_light: customdomain circuit breaker tripped\n"+
				"tenant: %s\nhost: %s\nfailures (1h window): %d\nfreeze: 24h",
			tenantID, host, failures,
		),
	}
	_ = w.post(ctx, body)
}

// post is the wire transport. Exposed for tests via PostForTest.
func (w *Webhook) post(ctx context.Context, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("slack: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack: status %d", resp.StatusCode)
	}
	return nil
}

// PostForTest is the test-only export of the wire path. It is here so
// the adapter has a non-fire-and-forget code path coverable by unit
// tests; production callers go through AlertCircuitTripped.
func (w *Webhook) PostForTest(ctx context.Context, text string) error {
	return w.post(ctx, map[string]string{"text": text})
}
