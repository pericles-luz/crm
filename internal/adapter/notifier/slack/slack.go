// Package slack implements slugreservation.SlackNotifier as a thin
// HTTPS POST to a Slack incoming-webhook URL.
//
// The adapter purposefully knows nothing about which channel it posts
// to — that is encoded in the webhook URL. A second alert sink (PagerDuty,
// Opsgenie) would live in a sibling package.
//
// Failures are returned to the caller; the use-case currently swallows
// them so a downed Slack does not roll back a master action, but
// callers are free to do otherwise.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// MaxRequestTimeout is the upper bound on a single Notify round-trip.
// Slack incoming-webhook responses are tiny; anything past 5 s is a
// hung peer, not a slow one.
const MaxRequestTimeout = 5 * time.Second

// Notifier sends short text alerts to Slack via webhook URL.
type Notifier struct {
	url     string
	channel string
	client  *http.Client
}

// New returns a Notifier. webhookURL must be HTTPS and non-empty;
// channel is the human-friendly tag prepended to the alert body
// (e.g. "#alerts").
func New(webhookURL, channel string, client *http.Client) (*Notifier, error) {
	if webhookURL == "" {
		return nil, errors.New("slack: webhook URL required")
	}
	if client == nil {
		client = &http.Client{Timeout: MaxRequestTimeout}
	}
	return &Notifier{url: webhookURL, channel: channel, client: client}, nil
}

type webhookBody struct {
	Channel string `json:"channel,omitempty"`
	Text    string `json:"text"`
}

// NotifyAlert posts a single chat message to the configured webhook.
// Returns an error on non-2xx HTTP, transport failure, or marshal
// failure.
func (n *Notifier) NotifyAlert(ctx context.Context, msg string) error {
	body, err := json.Marshal(webhookBody{Channel: n.channel, Text: msg})
	if err != nil {
		return fmt.Errorf("slack: marshal: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, MaxRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack: unexpected status %d", resp.StatusCode)
	}
	return nil
}
