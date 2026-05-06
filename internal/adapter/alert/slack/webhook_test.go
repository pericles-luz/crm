package slack_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/alert/slack"
)

func TestWebhook_AlertCircuitTrippedPostsToURL(t *testing.T) {
	t.Parallel()
	var seenBody string
	var seenContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		seenContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, err := slack.New(srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wh.AlertCircuitTripped(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001"), "shop.example.com", 7)
	if seenContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", seenContentType)
	}
	if !strings.Contains(seenBody, "circuit breaker tripped") {
		t.Fatalf("body missing tripped marker: %s", seenBody)
	}
	if !strings.Contains(seenBody, "shop.example.com") {
		t.Fatalf("body missing host: %s", seenBody)
	}
}

func TestWebhook_RejectsEmptyURL(t *testing.T) {
	t.Parallel()
	if _, err := slack.New("", nil); !errors.Is(err, slack.ErrEmptyURL) {
		t.Fatalf("err = %v, want ErrEmptyURL", err)
	}
}

type recordingClient struct {
	req *http.Request
	res *http.Response
	err error
}

func (c *recordingClient) Do(req *http.Request) (*http.Response, error) {
	c.req = req
	return c.res, c.err
}

func TestWebhook_AcceptsCustomHTTPClient(t *testing.T) {
	t.Parallel()
	rc := &recordingClient{
		res: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))},
	}
	wh, err := slack.New("https://hooks.slack.invalid/services/T/B/X", rc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := wh.PostForTest(context.Background(), "hello"); err != nil {
		t.Fatalf("PostForTest: %v", err)
	}
	if rc.req == nil || rc.req.URL.String() != "https://hooks.slack.invalid/services/T/B/X" {
		t.Fatalf("unexpected URL: %v", rc.req)
	}
}

func TestWebhook_NonSuccessStatusReturnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	wh, err := slack.New(srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := wh.PostForTest(context.Background(), "hello"); err == nil {
		t.Fatal("expected error on non-2xx response")
	}
}

func TestWebhook_HTTPClientErrorBubbles(t *testing.T) {
	t.Parallel()
	rc := &recordingClient{err: errors.New("net: timeout")}
	wh, err := slack.New("https://hooks.slack.invalid/services/T/B/X", rc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := wh.PostForTest(context.Background(), "hello"); err == nil {
		t.Fatal("expected error when HTTP client fails")
	}
}

func TestWebhook_BadURLReturnsError(t *testing.T) {
	t.Parallel()
	rc := &recordingClient{}
	wh, err := slack.New("://not-a-valid-url", rc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := wh.PostForTest(context.Background(), "hello"); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestWebhook_DefaultClientHasTimeout(t *testing.T) {
	t.Parallel()
	// No assertion possible without reflection — just make sure New
	// returns successfully with nil client.
	wh, err := slack.New("https://hooks.slack.invalid/services/T/B/X", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Use a deadline-already-passed ctx so the post short-circuits
	// quickly (avoids actually hitting the network).
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := wh.PostForTest(ctx, "hello"); err == nil {
		t.Fatal("expected error with pre-cancelled context")
	}
}
