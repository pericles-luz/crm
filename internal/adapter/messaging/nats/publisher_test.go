package nats_test

import (
	"context"
	"errors"
	"testing"
	"time"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/webhook"
)

type fakeJS struct {
	cfg     natsadapter.StreamConfig
	infoErr error
	pubErr  error
	pubArgs []struct {
		Subject string
		MsgID   string
		Body    []byte
	}
}

func (f *fakeJS) StreamInfo(context.Context, string) (natsadapter.StreamConfig, error) {
	return f.cfg, f.infoErr
}
func (f *fakeJS) Publish(_ context.Context, subject, msgID string, body []byte) error {
	f.pubArgs = append(f.pubArgs, struct {
		Subject string
		MsgID   string
		Body    []byte
	}{subject, msgID, body})
	return f.pubErr
}

func TestNew_ValidatesNilJS(t *testing.T) {
	t.Parallel()
	if _, err := natsadapter.New(context.Background(), nil, "x", "x."); err == nil {
		t.Fatal("expected error on nil JetStream")
	}
}

func TestNew_ValidatesEmptyStream(t *testing.T) {
	t.Parallel()
	js := &fakeJS{cfg: natsadapter.StreamConfig{Duplicates: time.Hour}}
	if _, err := natsadapter.New(context.Background(), js, "", "x."); err == nil {
		t.Fatal("expected error on empty stream name")
	}
}

func TestValidateStream_FailFastBelowMin(t *testing.T) {
	t.Parallel()
	js := &fakeJS{cfg: natsadapter.StreamConfig{Name: "wh", Duplicates: 2 * time.Minute}}
	_, err := natsadapter.New(context.Background(), js, "wh", "wh.")
	if err == nil {
		t.Fatal("expected fail-fast on Duplicates < 1h (F-14 invariant)")
	}
	if !contains(err.Error(), "F-14 prereq") {
		t.Fatalf("error %q does not point to F-14 prereq", err)
	}
}

func TestValidateStream_AcceptsExactlyMin(t *testing.T) {
	t.Parallel()
	js := &fakeJS{cfg: natsadapter.StreamConfig{Name: "wh", Duplicates: time.Hour}}
	if _, err := natsadapter.New(context.Background(), js, "wh", "wh."); err != nil {
		t.Fatalf("Duplicates == 1h must be accepted: %v", err)
	}
}

func TestValidateStream_StreamInfoError(t *testing.T) {
	t.Parallel()
	js := &fakeJS{infoErr: errors.New("stream not found")}
	if _, err := natsadapter.New(context.Background(), js, "wh", "wh."); err == nil {
		t.Fatal("expected error when StreamInfo fails")
	}
}

func TestPublisher_Publish(t *testing.T) {
	t.Parallel()
	js := &fakeJS{cfg: natsadapter.StreamConfig{Name: "wh", Duplicates: time.Hour}}
	pub, err := natsadapter.New(context.Background(), js, "wh", "wh.")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := [16]byte{1, 2, 3}
	if err := pub.Publish(context.Background(), id, webhook.TenantID{0xaa}, "whatsapp", []byte("payload"), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(js.pubArgs) != 1 {
		t.Fatalf("publish call count = %d, want 1", len(js.pubArgs))
	}
	got := js.pubArgs[0]
	if got.Subject != "wh.whatsapp" {
		t.Fatalf("subject = %q, want wh.whatsapp", got.Subject)
	}
	if got.MsgID == "" {
		t.Fatal("msgID must be set for JetStream dedup")
	}
}

func TestPublisher_PublishPropagatesError(t *testing.T) {
	t.Parallel()
	js := &fakeJS{
		cfg:    natsadapter.StreamConfig{Name: "wh", Duplicates: time.Hour},
		pubErr: errors.New("nats down"),
	}
	pub, err := natsadapter.New(context.Background(), js, "wh", "wh.")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := pub.Publish(context.Background(), [16]byte{}, webhook.TenantID{}, "whatsapp", nil, nil); err == nil {
		t.Fatal("expected publish error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
