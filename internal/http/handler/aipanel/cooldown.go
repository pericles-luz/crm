// Package aipanel renders the server-side HTMX fragments that back the AI
// panel UI. SIN-62238 introduces the cooldown fragment: when the rate
// limit middleware rejects a regenerate-button click it returns this
// fragment as the response body, and HTMX swaps it into the existing
// button slot in place of the live button — disabled, with a CSS-driven
// countdown.
//
// The countdown is implemented entirely with a CSS custom property and
// `animation` (no JS). The middleware sets two response headers:
//
//   - Retry-After: integer seconds (HTTP standard)
//   - X-RateLimit-Retry-After-Ms: integer milliseconds (precision for CSS)
//
// CooldownFragment reads those headers (or accepts the values as
// arguments when called directly) and emits the disabled-button HTML.
package aipanel

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"time"
)

// fragmentTpl is a small, audited template. The countdown bar's
// duration is encoded as `data-cooldown-bucket="N"` and matched against
// per-second selectors in `web/static/css/aipanel.css`. SIN-62319: an
// inline-style attribute would be dropped by the F29 CSP (style-src
// 'self' 'nonce-{N}'), so we use a data-* attribute that the CSP does
// not govern. Both the bucket and the visible "N s" caption derive from
// the same wall-clock value the middleware returned in Retry-After.
var fragmentTpl = template.Must(template.New("cooldown").Parse(`<button id="ai-panel-regenerate"
        class="ai-panel-cooldown"
        type="button"
        disabled
        aria-disabled="true"
        aria-live="polite"
        data-reason="{{.Reason}}"
        data-cooldown-bucket="{{.CooldownBucket}}"
        hx-disable-elt="this">
  <span class="ai-panel-cooldown__bar" aria-hidden="true"></span>
  <span class="ai-panel-cooldown__label">{{.Label}}</span>
</button>
`))

// fragmentData holds the values fed into fragmentTpl. CooldownBucket is
// the per-second bucket name ("1".."60" or "overflow") matched by the
// stylesheet to a `--cooldown-duration` CSS custom property.
type fragmentData struct {
	CooldownBucket string
	Label          string
	Reason         string
}

// CooldownFragment writes the HTMX fragment to w. retryAfter is the
// wall-clock duration the user must wait; reason is "quota" for normal
// rate-limit denials and "backend_unavailable" when the limiter backend
// failed. The two reasons render different copy so the user can tell a
// "you clicked too fast" cooldown from a "service is degraded" pause.
//
// The function sets Content-Type to text/html; charset=utf-8 if the
// caller has not already done so. It does NOT set status — the middleware
// owns the 429/503 status code and the Retry-After header.
func CooldownFragment(w io.Writer, retryAfter time.Duration, reason string) error {
	if retryAfter < 0 {
		retryAfter = 0
	}
	secs := int(retryAfter.Seconds())
	if retryAfter%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}

	label := fmt.Sprintf("Próxima geração em %d s", secs)
	if reason == "backend_unavailable" {
		label = fmt.Sprintf("AI panel indisponível. Tente em %d s", secs)
	}

	if rw, ok := w.(http.ResponseWriter); ok {
		if rw.Header().Get("Content-Type") == "" {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
	}

	return fragmentTpl.Execute(w, fragmentData{
		CooldownBucket: bucketFromMs(retryAfter.Milliseconds()),
		Label:          label,
		Reason:         reason,
	})
}

// CooldownRenderer satisfies the ratelimit.CooldownRenderer signature. It
// is the wiring point a server's main() uses to plug the AI panel
// cooldown fragment into the rate-limit middleware.
func CooldownRenderer(w http.ResponseWriter, _ *http.Request, retry time.Duration, reason string) {
	// retry validity is enforced by CooldownFragment; ignore its error so
	// a template glitch does not double-write the response (status was
	// already set by the middleware).
	_ = CooldownFragment(w, retry, reason)
}

// FragmentFromHeaders renders the cooldown fragment using the values the
// middleware already wrote into response headers, e.g. when a separate
// HTMX endpoint serves the fragment as its own route. headerRetryAfter
// is the integer seconds value of Retry-After; headerRetryAfterMs is the
// optional X-RateLimit-Retry-After-Ms header (use "" if absent — falls
// back to seconds * 1000).
func FragmentFromHeaders(w io.Writer, headerRetryAfter, headerRetryAfterMs, reason string) error {
	var retry time.Duration
	if ms, err := strconv.ParseInt(headerRetryAfterMs, 10, 64); err == nil && ms > 0 {
		retry = time.Duration(ms) * time.Millisecond
	} else if secs, err := strconv.Atoi(headerRetryAfter); err == nil && secs > 0 {
		retry = time.Duration(secs) * time.Second
	} else {
		retry = time.Second
	}
	return CooldownFragment(w, retry, reason)
}
