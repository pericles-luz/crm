package aipanel

// SIN-62319 — cooldown bucket selection.
//
// The cooldown fragment used to emit an inline-style attribute setting
// --cooldown-duration to retryAfter milliseconds, matching the server's
// Retry-After to the millisecond. The F29 CSP (style-src 'self'
// 'nonce-{N}') drops every inline style attribute, so we can no longer
// encode the duration that way.
//
// Instead, the fragment now carries `data-cooldown-bucket="N"` (a `data-*`
// attribute is not governed by style-src), and the static stylesheet pairs
// each bucket with a --cooldown-duration value. Buckets are per-second
// integers `1`..`60` plus `overflow` for anything > 60s. The visible
// integer-second label remains the source of truth for the user-facing
// countdown; the bar is decorative.

import "strconv"

const (
	cooldownMaxBucketSeconds = 60
	cooldownOverflowBucket   = "overflow"
)

// bucketFromMs returns the cooldown bucket name for the given retry
// duration in milliseconds. It rounds up (ceil) so the bar never finishes
// before the server-clock Retry-After actually elapses, and floors at
// `"1"` so a 0 ms or negative input still yields a valid bucket.
//
// Returns one of `"1"` .. `"60"` or `"overflow"`. The matching CSS lives
// in web/static/css/aipanel.css.
func bucketFromMs(ms int64) string {
	if ms <= 1000 {
		return "1"
	}
	secs := ms / 1000
	if ms%1000 != 0 {
		secs++
	}
	if secs > cooldownMaxBucketSeconds {
		return cooldownOverflowBucket
	}
	return strconv.FormatInt(secs, 10)
}
