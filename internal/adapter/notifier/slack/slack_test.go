package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	slacknotify "github.com/pericles-luz/crm/internal/adapter/notifier/slack"
)

func TestNotifyAlert_PostsToWebhook(t *testing.T) {
	t.Parallel()
	var got struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type=%q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := slacknotify.New(srv.URL, "#alerts", srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := n.NotifyAlert(context.Background(), "hi"); err != nil {
		t.Fatalf("NotifyAlert: %v", err)
	}
	if got.Channel != "#alerts" || got.Text != "hi" {
		t.Fatalf("got=%+v", got)
	}
}

func TestNotifyAlert_Non2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	n, err := slacknotify.New(srv.URL, "#alerts", srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := n.NotifyAlert(context.Background(), "hi"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNew_RequiresURL(t *testing.T) {
	t.Parallel()
	if _, err := slacknotify.New("", "#alerts", nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestNotifyAlert_TransportError(t *testing.T) {
	t.Parallel()
	// Use a closed listener URL so Dial fails immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()
	n, err := slacknotify.New(url, "#alerts", &http.Client{Transport: failingTransport{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := n.NotifyAlert(context.Background(), "hi"); err == nil {
		t.Fatal("expected transport error")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport down")
}

func TestNotifyAlert_BadURL(t *testing.T) {
	t.Parallel()
	// http.NewRequestWithContext returns an error for control-character URLs.
	n, err := slacknotify.New("http://\x7f", "#alerts", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := n.NotifyAlert(context.Background(), "hi"); err == nil {
		t.Fatal("expected error")
	}
}
