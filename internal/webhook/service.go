package webhook

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

// Service orchestrates a webhook intake request through the pipeline:
//
//	(1) lookup token → tenant
//	(2) verify HMAC (app- or tenant-scoped per adapter)
//	(3) cross-check body↔tenant association (rev 3 / F-12)
//	(4) extract + validate timestamp window
//	(5) compute idempotency key + INSERT … ON CONFLICT
//	(6) INSERT raw_event (published_at=NULL)
//	(7) ack 200 OK to caller
//	(8) async publish; on success, MarkPublished
//
// The service never imports database/sql, net/http, or any channel SDK.
// All side-effects flow through the ports declared in ports.go.
type Service struct {
	adapters     map[string]ChannelAdapter
	tokens       TokenStore
	idem         IdempotencyStore
	rawEvents    RawEventStore
	publisher    EventPublisher
	associations TenantAssociationStore
	clock        Clock
	logger       Logger
	metrics      Metrics
	pastWindow   time.Duration
	futureSkew   time.Duration
	publishCtx   func() context.Context
	asyncRunner  func(func())
}

// Config bundles the dependencies required to construct a Service. All
// fields except Adapters, TokenStore, IdempotencyStore, RawEventStore,
// Publisher, and Clock have safe defaults.
type Config struct {
	Adapters               []ChannelAdapter
	TokenStore             TokenStore
	IdempotencyStore       IdempotencyStore
	RawEventStore          RawEventStore
	Publisher              EventPublisher
	TenantAssociationStore TenantAssociationStore // rev 3 / F-12
	Clock                  Clock
	Logger                 Logger
	Metrics                Metrics

	// PastWindow accepts payloads up to this far in the past. ADR §2 D3
	// default = 5 minutes.
	PastWindow time.Duration
	// FutureSkew accepts payloads up to this far in the future (clock
	// skew). ADR §2 D3 default = 1 minute.
	FutureSkew time.Duration

	// AsyncRunner schedules the publish step. Defaults to `go func()`;
	// tests may pass a synchronous runner for deterministic assertions.
	AsyncRunner func(func())
	// PublishContext returns the context used for the async publish.
	// Defaults to context.Background() so request-cancel does not abort
	// the publish path.
	PublishContext func() context.Context
}

// NewService validates the configuration, registers adapters with
// fail-fast semantics (ADR §2 D4), and returns a ready Service. Error
// paths return *typed* validation errors so cmd/server can log a clean
// message and exit non-zero on misconfiguration.
func NewService(cfg Config) (*Service, error) {
	if cfg.TokenStore == nil {
		return nil, errors.New("webhook: TokenStore is required")
	}
	if cfg.IdempotencyStore == nil {
		return nil, errors.New("webhook: IdempotencyStore is required")
	}
	if cfg.RawEventStore == nil {
		return nil, errors.New("webhook: RawEventStore is required")
	}
	if cfg.Publisher == nil {
		return nil, errors.New("webhook: Publisher is required")
	}
	if cfg.TenantAssociationStore == nil {
		return nil, errors.New("webhook: TenantAssociationStore is required (rev 3 / F-12)")
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = noopLogger{}
	}
	if cfg.Metrics == nil {
		cfg.Metrics = noopMetrics{}
	}
	if cfg.PastWindow == 0 {
		cfg.PastWindow = 5 * time.Minute
	}
	if cfg.FutureSkew == 0 {
		cfg.FutureSkew = time.Minute
	}
	if cfg.AsyncRunner == nil {
		cfg.AsyncRunner = func(f func()) { go f() }
	}
	if cfg.PublishContext == nil {
		cfg.PublishContext = context.Background
	}

	s := &Service{
		adapters:     make(map[string]ChannelAdapter, len(cfg.Adapters)),
		tokens:       cfg.TokenStore,
		idem:         cfg.IdempotencyStore,
		rawEvents:    cfg.RawEventStore,
		publisher:    cfg.Publisher,
		associations: cfg.TenantAssociationStore,
		clock:        cfg.Clock,
		logger:       cfg.Logger,
		metrics:      cfg.Metrics,
		pastWindow:   cfg.PastWindow,
		futureSkew:   cfg.FutureSkew,
		publishCtx:   cfg.PublishContext,
		asyncRunner:  cfg.AsyncRunner,
	}
	for _, a := range cfg.Adapters {
		if err := s.registerAdapter(a); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// registerAdapter validates and inserts a single adapter. Exposed only
// for use by NewService; runtime registration is intentionally not
// supported (fail-fast at startup is the contract).
func (s *Service) registerAdapter(a ChannelAdapter) error {
	if a == nil {
		return errors.New("webhook: nil adapter")
	}
	name := a.Name()
	if err := ValidateChannelName(name); err != nil {
		return fmt.Errorf("webhook: adapter %T: %w", a, err)
	}
	if err := ValidateSecretScope(a.SecretScope()); err != nil {
		return fmt.Errorf("webhook: adapter %q: %w", name, err)
	}
	if _, dup := s.adapters[name]; dup {
		return fmt.Errorf("webhook: adapter %q registered twice", name)
	}
	s.adapters[name] = a
	return nil
}

// HasAdapter reports whether the channel name is registered. Used by
// the HTTP handler to short-circuit unknown channels with a silent 200.
func (s *Service) HasAdapter(channel string) bool {
	_, ok := s.adapters[channel]
	return ok
}

// Request bundles the inputs the HTTP handler hands to the service.
// Body is the raw bytes that fed both HMAC verification and idempotency
// hashing — re-reading r.Body after the handler computed Body is
// forbidden (ADR §2 D2 body bit-exactness).
type Request struct {
	Channel   string
	Token     string
	Body      []byte
	Headers   map[string][]string
	RequestID string
}

// Result is what the service returns to the handler. The handler always
// responds 200 OK regardless of Outcome (ADR §2 D5 anti-enumeration);
// callers use the outcome only for logging/metrics.
type Result struct {
	Outcome  Outcome
	TenantID TenantID
	Err      error
}

// Handle is the request entrypoint. Always returns a Result (never an
// error) — the handler always writes 200 + drop on non-accepted
// outcomes per ADR §2 D5.
func (s *Service) Handle(ctx context.Context, req Request) Result {
	start := s.clock.Now()
	defer func() {
		s.metrics.ObserveAck(req.Channel, s.clock.Now().Sub(start))
	}()

	adapter, ok := s.adapters[req.Channel]
	if !ok {
		return s.finishPreAuth(ctx, req, OutcomeUnknownChannel, nil)
	}

	tokenHash := sha256Sum(req.Token)
	now := s.clock.Now()

	switch adapter.SecretScope() {
	case SecretScopeApp:
		// HMAC verifies *before* TokenStore is touched (ADR §2 D4).
		if err := adapter.VerifyApp(ctx, req.Body, req.Headers); err != nil {
			return s.finishPreAuth(ctx, req, OutcomeSignatureInvalid, err)
		}
		tenantID, lookupErr := s.tokens.Lookup(ctx, req.Channel, tokenHash[:], now)
		if outcome := classifyTokenError(lookupErr); outcome != "" {
			return s.finishPreAuth(ctx, req, outcome, lookupErr)
		}
		return s.proceedAuthenticated(ctx, adapter, req, tenantID, tokenHash[:], now, start)

	case SecretScopeTenant:
		// Tenant resolved first, then HMAC verifies. Pre-HMAC, the
		// tenantID is a *claim* and MUST NOT be logged/metricized.
		tenantID, lookupErr := s.tokens.Lookup(ctx, req.Channel, tokenHash[:], now)
		if outcome := classifyTokenError(lookupErr); outcome != "" {
			return s.finishPreAuth(ctx, req, outcome, lookupErr)
		}
		if err := adapter.VerifyTenant(ctx, tenantID, req.Body, req.Headers); err != nil {
			return s.finishPreAuth(ctx, req, OutcomeSignatureInvalid, err)
		}
		return s.proceedAuthenticated(ctx, adapter, req, tenantID, tokenHash[:], now, start)

	default:
		// ValidateSecretScope at startup forbids this branch; defensive.
		return s.finishPreAuth(ctx, req, OutcomeInternalError, ErrUnsupportedScope)
	}
}

func (s *Service) proceedAuthenticated(
	ctx context.Context,
	adapter ChannelAdapter,
	req Request,
	tenantID TenantID,
	tokenHash []byte,
	now time.Time,
	start time.Time,
) Result {
	authCtx := withAuthenticatedTenant(ctx, tenantID)

	// rev 3 / F-12 — body↔tenant cross-check. Adapter declares the
	// association field via BodyTenantAssociation; if present, it MUST
	// belong to the URL-resolved tenant in tenant_channel_associations.
	// Adapters that have no usable association field return ok=false
	// and the check is skipped (documented per-adapter).
	if assoc, ok := adapter.BodyTenantAssociation(req.Body); ok {
		matched, err := s.associations.CheckAssociation(authCtx, tenantID, req.Channel, assoc)
		if err != nil {
			return s.finishAuth(authCtx, req, tenantID, OutcomeInternalError, err)
		}
		if !matched {
			return s.finishAuth(authCtx, req, tenantID, OutcomeTenantBodyMismatch, nil)
		}
	}

	ts, err := adapter.ExtractTimestamp(req.Headers, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, ErrTimestampMissing):
			return s.finishAuth(authCtx, req, tenantID, OutcomeTimestampMissing, err)
		case errors.Is(err, ErrTimestampFormat):
			return s.finishAuth(authCtx, req, tenantID, OutcomeTimestampFormatError, err)
		default:
			return s.finishAuth(authCtx, req, tenantID, OutcomeTimestampFormatError, err)
		}
	}
	if !s.timestampInWindow(ts, now) {
		return s.finishAuth(authCtx, req, tenantID, OutcomeReplayWindowViolation, ErrTimestampOutOfRange)
	}

	idemKey := computeIdempotencyKey(tenantID, req.Channel, req.Body)
	firstSeen, err := s.idem.CheckAndStore(authCtx, tenantID, req.Channel, idemKey[:], now)
	if err != nil {
		return s.finishAuth(authCtx, req, tenantID, OutcomeInternalError, err)
	}
	if !firstSeen {
		s.metrics.IncIdempotencyConflict(req.Channel, tenantID)
		return s.finishAuth(authCtx, req, tenantID, OutcomeReplay, nil)
	}

	if _, err := adapter.ParseEvent(req.Body); err != nil {
		return s.finishAuth(authCtx, req, tenantID, OutcomeParseError, err)
	}

	row := RawEventRow{
		TenantID:       tenantID,
		Channel:        req.Channel,
		IdempotencyKey: idemKey[:],
		Payload:        req.Body,
		Headers:        req.Headers,
		ReceivedAt:     now,
	}
	eventID, err := s.rawEvents.Insert(authCtx, row)
	if err != nil {
		return s.finishAuth(authCtx, req, tenantID, OutcomeInternalError, err)
	}

	// Best-effort token usage marker — never fatal.
	_ = s.tokens.MarkUsed(authCtx, req.Channel, tokenHash, now)

	// Async publish. The reconciler (ADR §2 D7) catches us up if this
	// path crashes between Insert and MarkPublished.
	publisher := s.publisher
	rawEvents := s.rawEvents
	clock := s.clock
	publishCtxFn := s.publishCtx
	channel := req.Channel
	headers := req.Headers
	body := req.Body
	s.asyncRunner(func() {
		ctx := publishCtxFn()
		if err := publisher.Publish(ctx, eventID, tenantID, channel, body, headers); err != nil {
			return
		}
		_ = rawEvents.MarkPublished(ctx, eventID, clock.Now())
	})

	_ = start
	return s.finishAuth(authCtx, req, tenantID, OutcomeAccepted, nil)
}

func (s *Service) finishPreAuth(ctx context.Context, req Request, outcome Outcome, err error) Result {
	s.metrics.IncReceived(req.Channel, outcome, TenantID{}, false)
	s.logger.LogResult(ctx, LogRecord{
		RequestID:  req.RequestID,
		Channel:    req.Channel,
		Outcome:    outcome,
		ReceivedAt: s.clock.Now(),
		Err:        err,
	})
	return Result{Outcome: outcome, Err: err}
}

func (s *Service) finishAuth(
	ctx context.Context,
	req Request,
	tenantID TenantID,
	outcome Outcome,
	err error,
) Result {
	s.metrics.IncReceived(req.Channel, outcome, tenantID, outcome.IsAuthenticated())
	s.logger.LogResult(ctx, LogRecord{
		RequestID:   req.RequestID,
		Channel:     req.Channel,
		Outcome:     outcome,
		ReceivedAt:  s.clock.Now(),
		TenantID:    tenantID,
		HasTenantID: outcome.IsAuthenticated(),
		Err:         err,
	})
	return Result{Outcome: outcome, TenantID: tenantID, Err: err}
}

func (s *Service) timestampInWindow(ts, now time.Time) bool {
	if ts.Before(now.Add(-s.pastWindow)) {
		return false
	}
	if ts.After(now.Add(s.futureSkew)) {
		return false
	}
	return true
}

func classifyTokenError(err error) Outcome {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrTokenUnknown):
		return OutcomeUnknownToken
	case errors.Is(err, ErrTokenRevoked):
		return OutcomeRevokedToken
	default:
		return OutcomeInternalError
	}
}

// sha256Sum hashes a string (typically the bearer-style webhook token).
// Returned as a fixed-width array so callers don't accidentally reslice.
func sha256Sum(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

// computeIdempotencyKey hashes (tenant_id || ':' || channel || ':' ||
// raw_payload) per ADR §2 D2. The colon delimiter is unambiguous because
// channel ∈ [a-z0-9_]+ (no `:`) and tenant_id is fixed-width.
func computeIdempotencyKey(tenantID TenantID, channel string, payload []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write(tenantID[:])
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(channel))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write(payload)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// noopLogger / noopMetrics are zero-cost defaults for embedded use.
type noopLogger struct{}

func (noopLogger) LogResult(context.Context, LogRecord) {}

type noopMetrics struct{}

func (noopMetrics) IncReceived(string, Outcome, TenantID, bool) {}
func (noopMetrics) ObserveAck(string, time.Duration)            {}
func (noopMetrics) IncIdempotencyConflict(string, TenantID)     {}
