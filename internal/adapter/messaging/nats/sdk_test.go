package nats

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

type fakeJSContext struct {
	infoCalls []string
	infoResp  *natsgo.StreamInfo
	infoErr   error

	pubCalls []capturedPub
	pubResp  *natsgo.PubAck
	pubErr   error
}

type capturedPub struct {
	Subject string
	Body    []byte
	NumOpts int
}

func (f *fakeJSContext) StreamInfo(stream string, _ ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	f.infoCalls = append(f.infoCalls, stream)
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	return f.infoResp, nil
}

func (f *fakeJSContext) Publish(subj string, data []byte, opts ...natsgo.PubOpt) (*natsgo.PubAck, error) {
	f.pubCalls = append(f.pubCalls, capturedPub{Subject: subj, Body: append([]byte(nil), data...), NumOpts: len(opts)})
	if f.pubErr != nil {
		return nil, f.pubErr
	}
	return f.pubResp, nil
}

// jsContextStub satisfies natsgo.JetStreamContext by embedding the
// interface (any unstubbed method panics if called) and delegating the
// two methods SDKAdapter actually uses to the package-local fake.
type jsContextStub struct {
	natsgo.JetStreamContext
	inner *fakeJSContext
}

func (s *jsContextStub) StreamInfo(stream string, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	return s.inner.StreamInfo(stream, opts...)
}

func (s *jsContextStub) Publish(subj string, data []byte, opts ...natsgo.PubOpt) (*natsgo.PubAck, error) {
	return s.inner.Publish(subj, data, opts...)
}

type fakeConn struct {
	jsResp     natsgo.JetStreamContext
	jsErr      error
	jsCalls    int
	drainCalls int
	drainErr   error
}

func (f *fakeConn) JetStream(_ ...natsgo.JSOpt) (natsgo.JetStreamContext, error) {
	f.jsCalls++
	return f.jsResp, f.jsErr
}

func (f *fakeConn) Drain() error {
	f.drainCalls++
	return f.drainErr
}

func newAdapter(js jsContext) *SDKAdapter {
	return &SDKAdapter{js: js}
}

func TestConnect_RejectsEmptyURL(t *testing.T) {
	t.Parallel()
	_, err := Connect(context.Background(), SDKConfig{})
	if err == nil {
		t.Fatal("expected error on empty URL")
	}
	if !strings.Contains(err.Error(), "URL is required") {
		t.Fatalf("error %q does not mention URL requirement", err)
	}
}

func TestConnect_DialFailureWraps(t *testing.T) {
	t.Parallel()
	// Unroutable port + 0 reconnects so Connect bails fast.
	_, err := Connect(context.Background(), SDKConfig{
		URL:           "nats://127.0.0.1:1",
		ReconnectWait: time.Millisecond,
		MaxReconnects: 0,
	})
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if !strings.Contains(err.Error(), "nats: connect") {
		t.Fatalf("error %q is not the wrapped dial error", err)
	}
}

func TestOptionsFromConfig_ProducesOptionsForEachField(t *testing.T) {
	t.Parallel()
	cfg := SDKConfig{
		URL:           "nats://example.invalid:4222",
		ClientName:    "crm-test",
		CredsFile:     "/nonexistent/creds.creds",
		TLSConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		ReconnectWait: 250 * time.Millisecond,
		MaxReconnects: -1,
	}
	opts := optionsFromConfig(cfg)
	if len(opts) != 5 {
		t.Fatalf("expected 5 options (Name, UserCredentials, Secure, ReconnectWait, MaxReconnects), got %d", len(opts))
	}
}

func TestOptionsFromConfig_EmptyConfigProducesNoOptions(t *testing.T) {
	t.Parallel()
	if got := optionsFromConfig(SDKConfig{URL: "nats://x"}); len(got) != 0 {
		t.Fatalf("expected zero options for bare config, got %d", len(got))
	}
}

func TestOptionsFromConfig_ZeroMaxReconnectsLeavesDefault(t *testing.T) {
	t.Parallel()
	got := optionsFromConfig(SDKConfig{ClientName: "x"})
	if len(got) != 1 {
		t.Fatalf("zero MaxReconnects must skip the option; got %d options", len(got))
	}
}

func TestWrap_Success(t *testing.T) {
	t.Parallel()
	stub := &jsContextStub{inner: &fakeJSContext{infoResp: &natsgo.StreamInfo{
		Config: natsgo.StreamConfig{Name: "wh", Duplicates: time.Hour},
	}}}
	conn := &fakeConn{jsResp: stub}

	a, err := wrap(conn)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if a == nil || a.conn == nil || a.js == nil {
		t.Fatal("wrap must populate conn + js")
	}
	if conn.jsCalls != 1 {
		t.Fatalf("JetStream() calls = %d, want 1", conn.jsCalls)
	}
	// Sanity: round-trip a StreamInfo through the wrapped adapter.
	cfg, err := a.StreamInfo(context.Background(), "wh")
	if err != nil || cfg.Name != "wh" || cfg.Duplicates != time.Hour {
		t.Fatalf("StreamInfo via wrapped adapter = (%+v, %v)", cfg, err)
	}
}

func TestWrap_JetStreamErrorDrainsConn(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{jsErr: errors.New("jetstream not enabled")}
	a, err := wrap(conn)
	if err == nil {
		t.Fatal("expected JetStream error to surface")
	}
	if a != nil {
		t.Fatal("wrap must not return an adapter on JetStream failure")
	}
	if conn.drainCalls != 1 {
		t.Fatalf("expected the conn to be drained once on failure; got %d Drain() calls", conn.drainCalls)
	}
	if !strings.Contains(err.Error(), "jetstream not enabled") {
		t.Fatalf("error %q does not wrap the underlying SDK error", err)
	}
}

func TestSDKAdapter_StreamInfo_MapsConfig(t *testing.T) {
	t.Parallel()
	js := &fakeJSContext{infoResp: &natsgo.StreamInfo{
		Config: natsgo.StreamConfig{Name: "wh", Duplicates: 90 * time.Minute},
	}}
	a := newAdapter(js)

	cfg, err := a.StreamInfo(context.Background(), "wh")
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if cfg.Name != "wh" || cfg.Duplicates != 90*time.Minute {
		t.Fatalf("mapped config = %+v, want {Name:wh Duplicates:1h30m}", cfg)
	}
	if len(js.infoCalls) != 1 || js.infoCalls[0] != "wh" {
		t.Fatalf("StreamInfo SDK calls = %v, want [wh]", js.infoCalls)
	}
}

func TestSDKAdapter_StreamInfo_PropagatesError(t *testing.T) {
	t.Parallel()
	js := &fakeJSContext{infoErr: errors.New("stream not found")}
	a := newAdapter(js)
	_, err := a.StreamInfo(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "stream not found") {
		t.Fatalf("expected wrapped stream-not-found error, got %v", err)
	}
}

func TestSDKAdapter_StreamInfo_HonorsCancelledContext(t *testing.T) {
	t.Parallel()
	js := &fakeJSContext{infoResp: &natsgo.StreamInfo{Config: natsgo.StreamConfig{Name: "wh", Duplicates: time.Hour}}}
	a := newAdapter(js)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.StreamInfo(ctx, "wh"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(js.infoCalls) != 0 {
		t.Fatal("must not call SDK after cancelled context")
	}
}

func TestSDKAdapter_Publish_PassesSubjectAndBody(t *testing.T) {
	t.Parallel()
	js := &fakeJSContext{pubResp: &natsgo.PubAck{Stream: "wh", Sequence: 42}}
	a := newAdapter(js)
	body := []byte("payload")
	if err := a.Publish(context.Background(), "wh.whatsapp", "deadbeef", body); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(js.pubCalls) != 1 {
		t.Fatalf("publish calls = %d, want 1", len(js.pubCalls))
	}
	got := js.pubCalls[0]
	if got.Subject != "wh.whatsapp" {
		t.Fatalf("subject = %q, want wh.whatsapp", got.Subject)
	}
	if string(got.Body) != "payload" {
		t.Fatalf("body = %q, want %q", got.Body, "payload")
	}
	if got.NumOpts != 2 {
		t.Fatalf("PubOpt count = %d, want 2 (MsgId + Context)", got.NumOpts)
	}
}

func TestSDKAdapter_Publish_RejectsEmptyMsgID(t *testing.T) {
	t.Parallel()
	js := &fakeJSContext{}
	a := newAdapter(js)
	err := a.Publish(context.Background(), "wh.whatsapp", "", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "msgID is required") {
		t.Fatalf("want msgID-required error, got %v", err)
	}
	if len(js.pubCalls) != 0 {
		t.Fatal("must not hit SDK without msgID (JetStream dedup invariant)")
	}
}

func TestSDKAdapter_Publish_PropagatesError(t *testing.T) {
	t.Parallel()
	js := &fakeJSContext{pubErr: errors.New("nats down")}
	a := newAdapter(js)
	err := a.Publish(context.Background(), "wh.whatsapp", "id", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "nats down") {
		t.Fatalf("expected wrapped nats-down error, got %v", err)
	}
}

func TestSDKAdapter_Publish_HonorsCancelledContext(t *testing.T) {
	t.Parallel()
	js := &fakeJSContext{pubResp: &natsgo.PubAck{}}
	a := newAdapter(js)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Publish(ctx, "wh.whatsapp", "id", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(js.pubCalls) != 0 {
		t.Fatal("must not call SDK after cancelled context")
	}
}

func TestSDKAdapter_Close_DrainsConnAndIsIdempotent(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{}
	a := &SDKAdapter{conn: conn}
	a.Close()
	a.Close() // second call should be a no-op
	if conn.drainCalls != 1 {
		t.Fatalf("Drain() calls = %d, want exactly 1 across two Close() invocations", conn.drainCalls)
	}
}

func TestSDKAdapter_Close_NilSafe(t *testing.T) {
	t.Parallel()
	var a *SDKAdapter
	a.Close()
	(&SDKAdapter{}).Close() // zero-valued adapter (nil conn) is also safe
}

func TestSDKAdapter_SatisfiesJetStream(t *testing.T) {
	t.Parallel()
	var _ JetStream = (*SDKAdapter)(nil)
}
