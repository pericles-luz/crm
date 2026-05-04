package nats

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// SDKAdapter wires the official nats.go SDK to the package-local
// JetStream interface used by Publisher + ValidateStream. It implements
// JetStream by translating to/from `nats.go` types so the rest of the
// adapter package (and the webhook domain) stays SDK-agnostic.
type SDKAdapter struct {
	conn natsConn
	js   jsContext
}

// natsConn is the narrow surface SDKAdapter needs from *nats.Conn.
// Defining it here lets tests inject a fake conn without dialing a
// real server while still letting the production wire-up pass a
// real *natsgo.Conn (which already satisfies this interface).
type natsConn interface {
	JetStream(opts ...natsgo.JSOpt) (natsgo.JetStreamContext, error)
	Drain() error
}

// jsContext is the narrow surface SDKAdapter needs from the SDK's
// JetStreamContext. Used by Publish + StreamInfo and the
// post-dial wiring.
type jsContext interface {
	StreamInfo(stream string, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error)
	Publish(subj string, data []byte, opts ...natsgo.PubOpt) (*natsgo.PubAck, error)
}

// SDKConfig captures every connection knob needed to build an
// SDKAdapter. Keep secrets out of code: load CredsFile / TLS material
// from the runtime environment (file paths, secret manager, etc).
type SDKConfig struct {
	// URL is the NATS server URL (e.g. nats://host:4222 or
	// tls://host:4222). Required.
	URL string

	// ClientName surfaces in NATS server monitoring; helps operators
	// tell connections apart. Optional but recommended.
	ClientName string

	// CredsFile is a path to a NATS credentials file (JWT + nkey seed).
	// Empty means no credential auth. Never embed creds in code.
	CredsFile string

	// TLSConfig is applied verbatim when set. Use this for mTLS or
	// custom CA pools; the tls.Config.MinVersion default in stdlib is
	// already TLS 1.2.
	TLSConfig *tls.Config

	// ReconnectWait is the back-off between reconnect attempts.
	// Zero leaves the SDK default.
	ReconnectWait time.Duration

	// MaxReconnects bounds reconnect attempts. -1 means infinite
	// (matches the SDK default but stated explicitly here).
	MaxReconnects int
}

// Connect dials NATS using cfg, opens a JetStream context, and returns
// an SDKAdapter ready to satisfy the JetStream interface. Returns an
// error if cfg.URL is empty or the dial/JetStream handshake fails.
func Connect(_ context.Context, cfg SDKConfig) (*SDKAdapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("nats: SDKConfig.URL is required")
	}
	opts := optionsFromConfig(cfg)
	conn, err := natsgo.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect %q: %w", cfg.URL, err)
	}
	return wrap(conn)
}

// wrap turns an already-connected NATS conn into an SDKAdapter,
// failing fast if JetStream is not enabled on the server. Split out
// from Connect so it can be unit-tested against a fake conn.
func wrap(conn natsConn) (*SDKAdapter, error) {
	js, err := conn.JetStream()
	if err != nil {
		_ = conn.Drain()
		return nil, fmt.Errorf("nats: open JetStream context: %w", err)
	}
	return &SDKAdapter{conn: conn, js: js}, nil
}

// optionsFromConfig translates SDKConfig into nats.go options. Split
// out so it can be unit-tested without a live server.
func optionsFromConfig(cfg SDKConfig) []natsgo.Option {
	var opts []natsgo.Option
	if cfg.ClientName != "" {
		opts = append(opts, natsgo.Name(cfg.ClientName))
	}
	if cfg.CredsFile != "" {
		opts = append(opts, natsgo.UserCredentials(cfg.CredsFile))
	}
	if cfg.TLSConfig != nil {
		opts = append(opts, natsgo.Secure(cfg.TLSConfig))
	}
	if cfg.ReconnectWait > 0 {
		opts = append(opts, natsgo.ReconnectWait(cfg.ReconnectWait))
	}
	// 0 keeps SDK default; negative values are passed through so
	// callers can opt into infinite reconnects with -1.
	if cfg.MaxReconnects != 0 {
		opts = append(opts, natsgo.MaxReconnects(cfg.MaxReconnects))
	}
	return opts
}

// Close drains and clears the underlying NATS connection. Safe to
// call multiple times; subsequent calls are no-ops.
func (a *SDKAdapter) Close() {
	if a == nil || a.conn == nil {
		return
	}
	_ = a.conn.Drain()
	a.conn = nil
}

// StreamInfo implements JetStream by querying the JetStream context
// and mapping the SDK's StreamConfig to the package-local one.
func (a *SDKAdapter) StreamInfo(ctx context.Context, name string) (StreamConfig, error) {
	if err := ctx.Err(); err != nil {
		return StreamConfig{}, err
	}
	info, err := a.js.StreamInfo(name, natsgo.Context(ctx))
	if err != nil {
		return StreamConfig{}, fmt.Errorf("nats: StreamInfo %q: %w", name, err)
	}
	return StreamConfig{
		Name:       info.Config.Name,
		Duplicates: info.Config.Duplicates,
	}, nil
}

// Publish implements JetStream by forwarding to the SDK with the
// Nats-Msg-Id header populated for JetStream dedup (rev 3 / F-14).
// msgID is expected to be hex(idempotency_key) — Publisher sets this.
func (a *SDKAdapter) Publish(ctx context.Context, subject, msgID string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msgID == "" {
		return errors.New("nats: msgID is required for JetStream dedup")
	}
	opts := []natsgo.PubOpt{natsgo.MsgId(msgID), natsgo.Context(ctx)}
	if _, err := a.js.Publish(subject, body, opts...); err != nil {
		return fmt.Errorf("nats: Publish %q: %w", subject, err)
	}
	return nil
}
