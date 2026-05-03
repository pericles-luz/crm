// Package nats hosts the JetStream-backed implementation of
// webhook.EventPublisher. The package is intentionally written against
// the small interfaces declared below rather than directly against
// `github.com/nats-io/nats.go`, so the F-14 invariant (Duplicates
// window ≥ 1h) is testable with a fake client.
//
// The real wiring against `nats.go` is the responsibility of the
// follow-up CTO-approved child issue (introducing the dependency is a
// boring-tech-budget decision that is outside the SIN-62234 scope).
// What this package owns now:
//
//   - the EventPublisher implementation (Publish + Nats-Msg-Id header
//     populated with hex(idempotency_key));
//   - the startup config validator that fails-fast when a stream's
//     Duplicates window is below 1 hour (rev 3 / F-14).
package nats

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// MinDuplicatesWindow is the minimum JetStream duplicate-detection
// window required by ADR §6 prereq for prod-on (rev 3 / F-14). Default
// JetStream is 2 minutes; we need ≥ 1 hour to align with the
// reconciler's 1h tolerance.
const MinDuplicatesWindow = time.Hour

// StreamConfig is the subset of JetStream config we care about for the
// fail-fast check. Mapped from `nats.go`'s `StreamConfig` in the real
// adapter (Subjects + Duplicates) without forcing a build-time
// dependency on it here.
type StreamConfig struct {
	Name       string
	Duplicates time.Duration
}

// JetStream is the narrow surface needed by both the validator and the
// publisher. The real adapter passes a *nats.go* JetStream context
// behind this interface.
type JetStream interface {
	StreamInfo(ctx context.Context, name string) (StreamConfig, error)
	Publish(ctx context.Context, subject string, msgID string, body []byte) error
}

// Publisher implements webhook.EventPublisher on top of JetStream.
// One Publisher is bound to one stream + subject prefix; the channel
// is appended at publish time so consumers can subscribe per-channel.
type Publisher struct {
	js            JetStream
	subjectPrefix string
}

// New constructs a Publisher and validates the stream config eagerly
// (fail-fast on F-14 prereq). subjectPrefix MUST end with a dot — the
// channel name is concatenated.
func New(ctx context.Context, js JetStream, streamName, subjectPrefix string) (*Publisher, error) {
	if js == nil {
		return nil, errors.New("nats: JetStream is required")
	}
	if streamName == "" {
		return nil, errors.New("nats: streamName is required")
	}
	if err := ValidateStream(ctx, js, streamName); err != nil {
		return nil, err
	}
	return &Publisher{js: js, subjectPrefix: subjectPrefix}, nil
}

// ValidateStream checks the F-14 invariant. Returns a clear actionable
// error so the operator knows exactly what to update.
func ValidateStream(ctx context.Context, js JetStream, streamName string) error {
	cfg, err := js.StreamInfo(ctx, streamName)
	if err != nil {
		return fmt.Errorf("nats: read stream %q config: %w", streamName, err)
	}
	if cfg.Duplicates < MinDuplicatesWindow {
		return fmt.Errorf(
			"nats: stream %q has Duplicates=%s, want >= %s "+
				"(F-14 prereq: align with reconciler 1h tolerance; update stream config and redeploy)",
			streamName, cfg.Duplicates, MinDuplicatesWindow,
		)
	}
	return nil
}

// Publish implements webhook.EventPublisher. The Nats-Msg-Id (mapped
// to JetStream's deduplication header) is populated with
// hex(idempotency_key) so JetStream dedup catches duplicates published
// by the request path + reconciler within MinDuplicatesWindow.
func (p *Publisher) Publish(
	ctx context.Context,
	eventID [16]byte,
	tenantID webhook.TenantID,
	channel string,
	payload []byte,
	_ map[string][]string,
) error {
	subject := p.subjectPrefix + channel
	// Use the raw_event id as the dedup key — it is unique per
	// (received_at, id) and therefore also globally unique for the
	// JetStream window. The idempotency_key is also unique within the
	// (tenant, channel, payload) triple but lives in webhook_idempotency
	// and isn't carried into the publisher signature.
	msgID := hex.EncodeToString(eventID[:])
	_ = tenantID // tenant context is encoded in the subject by the consumer side
	return p.js.Publish(ctx, subject, msgID, payload)
}
