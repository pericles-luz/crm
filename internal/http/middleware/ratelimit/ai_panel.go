package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	aiport "github.com/pericles-luz/crm/internal/ai/port"
)

// Bucket name constants. Mirrored here (the canonical definitions live in
// the Redis adapter package) so the middleware does not depend on the
// concrete adapter — only on port.RateLimiter.
const (
	BucketUserConv = "ai:panel:user-conv"
	BucketUser     = "ai:panel:user"
)

// IdentityResolver extracts the (tenantID, userID, conversationID) tuple
// from an inbound request. Implementations typically read auth context
// (cookie/JWT) for tenant+user and a path/query parameter for the
// conversation. Returning an error MUST cause the middleware to reject the
// request with 401/400 — the Coder side defines a default that returns
// 401 when any field is empty.
type IdentityResolver func(*http.Request) (tenantID, userID, conversationID string, err error)

// CooldownRenderer optionally renders the HTMX/HTML fragment that the
// browser will swap into the regenerate-button slot when the request is
// rejected. If nil, the middleware writes a minimal text/plain body.
//
// retryAfter is the wall-clock duration the client should wait. reason is
// "quota" for quota denials and "backend_unavailable" for fail-closed
// returns; renderers can show different copy for each.
type CooldownRenderer func(w http.ResponseWriter, r *http.Request, retryAfter time.Duration, reason string)

// Config is the dependency bundle needed by Middleware. All fields are
// required except CooldownRenderer.
type Config struct {
	Limiter  aiport.RateLimiter
	Identity IdentityResolver
	Render   CooldownRenderer
}

// Middleware returns an http.Handler middleware that enforces the AI panel
// rate limits on every request that reaches it. Both stacked buckets are
// checked; on any denial the middleware answers without invoking next.
//
// Per SIN-62225 §3.6 the response on quota exhaustion is
// 429 Too Many Requests with Retry-After equal to max(retryAfterUserConv,
// retryAfterUser). When the limiter signals ErrLimiterUnavailable the
// middleware answers 503 (fail-closed) with Retry-After set to a
// conservative non-zero default — callers MUST NOT proceed to the AI panel.
func Middleware(cfg Config) func(http.Handler) http.Handler {
	if cfg.Limiter == nil {
		panic("ratelimit.Middleware: Config.Limiter is required")
	}
	if cfg.Identity == nil {
		panic("ratelimit.Middleware: Config.Identity is required")
	}
	render := cfg.Render
	if render == nil {
		render = defaultRenderer
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID, userID, convID, err := cfg.Identity(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if tenantID == "" || userID == "" || convID == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := r.Context()
			convKey := fmt.Sprintf("tenant:%s:user:%s:conv:%s", tenantID, userID, convID)
			userKey := fmt.Sprintf("tenant:%s:user:%s", tenantID, userID)

			allowedConv, retryConv, errConv := cfg.Limiter.Allow(ctx, BucketUserConv, convKey)
			allowedUser, retryUser, errUser := cfg.Limiter.Allow(ctx, BucketUser, userKey)

			// Fail-closed path: limiter is unavailable. Pick the bucket label
			// from whichever returned the error first so dashboards still
			// disambiguate. 503 distinguishes backend failure from quota.
			if errors.Is(errConv, aiport.ErrLimiterUnavailable) || errors.Is(errUser, aiport.ErrLimiterUnavailable) {
				bucket := BucketUserConv
				if errors.Is(errUser, aiport.ErrLimiterUnavailable) && !errors.Is(errConv, aiport.ErrLimiterUnavailable) {
					bucket = BucketUser
				}
				retry := maxDuration(retryConv, retryUser)
				if retry < time.Second {
					retry = time.Second
				}
				rateLimitedTotal.WithLabelValues(bucket, "backend_unavailable").Inc()
				writeRetryAfter(w, retry)
				w.WriteHeader(http.StatusServiceUnavailable)
				render(w, r, retry, "backend_unavailable")
				return
			}

			// Quota path: at least one bucket denied. Pick the denying bucket
			// (preferring user-conv when both deny — it is the more specific
			// signal). Retry-After is the max so the client waits long enough
			// for both buckets to recover.
			if !allowedConv || !allowedUser {
				bucket := BucketUserConv
				if allowedConv && !allowedUser {
					bucket = BucketUser
				}
				retry := maxDuration(retryConv, retryUser)
				if retry <= 0 {
					retry = time.Second
				}
				rateLimitedTotal.WithLabelValues(bucket, "quota").Inc()
				writeRetryAfter(w, retry)
				w.WriteHeader(http.StatusTooManyRequests)
				render(w, r, retry, "quota")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeRetryAfter sets the Retry-After header in seconds, rounded up so a
// value of 1.1s produces "2" (clients get the conservative wait).
func writeRetryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	// Surface the raw millisecond value too so the frontend cooldown CSS
	// animation can be precise without a second round trip. Custom header,
	// HTMX-friendly, ignored by older clients.
	w.Header().Set("X-RateLimit-Retry-After-Ms", strconv.FormatInt(d.Milliseconds(), 10))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// defaultRenderer writes a minimal text/plain body. Rich HTMX rendering
// is the FrontendCoder responsibility; see internal/http/handler/aipanel
// for the cooldown fragment renderer that satisfies SIN-62225 §3.6 (4).
func defaultRenderer(w http.ResponseWriter, _ *http.Request, retry time.Duration, reason string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if reason == "backend_unavailable" {
		fmt.Fprintf(w, "AI panel temporarily unavailable. Retry in %ds.\n", int(math.Ceil(retry.Seconds())))
		return
	}
	fmt.Fprintf(w, "Rate limit exceeded. Retry in %ds.\n", int(math.Ceil(retry.Seconds())))
}

// IdentityFromContext is a small helper for callers who already store the
// tuple on context.Context — it returns an IdentityResolver that fetches
// the values via the supplied accessor. Useful when an upstream auth
// middleware has already done the work.
func IdentityFromContext(get func(context.Context) (tenant, user, conv string, err error)) IdentityResolver {
	return func(r *http.Request) (string, string, string, error) {
		return get(r.Context())
	}
}
