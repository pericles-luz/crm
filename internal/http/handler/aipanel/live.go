package aipanel

// SIN-62317 — live (enabled) regenerate button.
//
// LiveButton renders the regenerate button in its ready-to-click state. It
// is the swap target the cooldown fragment in cooldown.go replaces when the
// rate-limit middleware rejects the request: the live button declares
// `id="ai-panel-regenerate"` and `hx-swap="outerHTML"`, and CooldownFragment
// emits the same id, so HTMX swaps the disabled fragment in place. Once the
// cooldown animation completes the host page can re-render this live state
// (typically by triggering a follow-up request after `Retry-After`).
//
// The CSS in web/static/css/aipanel.css gives both states the same outer
// box dimensions (padding + border + radius) so the swap does not cause
// layout shift (Core Web Vitals — CLS).

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
)

// DefaultStylesheetHref is the URL the existing /static/ FileServer (see
// cmd/server/customdomain_wire.go) maps to web/static/css/aipanel.css.
// Hosting templates should `<link rel="stylesheet">` to this href so the
// cooldown countdown animation runs in production.
const DefaultStylesheetHref = "/static/css/aipanel.css"

// LiveButtonOptions configures the LiveButton renderer.
//
// PostPath is the HTMX endpoint that triggers a panel regeneration. It is
// emitted as the `hx-post` attribute and is required — there is no sensible
// default because the route is owned by whichever package mounts the AI
// panel UI.
//
// Label is the visible button caption. Defaults to "Regenerar" when empty.
// Callers pass a tenant- or feature-specific label here when needed.
type LiveButtonOptions struct {
	PostPath string
	Label    string
}

// liveBtnData backs liveBtnTpl.
type liveBtnData struct {
	PostPath template.URL
	Label    string
}

// liveBtnTpl is the audited live-button template. Attribute names mirror
// the cooldown fragment so the outerHTML swap target is stable:
//
//   - id="ai-panel-regenerate" — swap target (matches cooldown.go)
//   - hx-target="#ai-panel-regenerate" + hx-swap="outerHTML"
//   - hx-disable-elt="this" — HTMX disables the element while the request
//     is in flight, mirroring the server-rendered disabled state on 429/503.
var liveBtnTpl = template.Must(template.New("aipanel-live").Parse(
	`<button id="ai-panel-regenerate"
        class="ai-panel-regenerate"
        type="button"
        hx-post="{{.PostPath}}"
        hx-target="#ai-panel-regenerate"
        hx-swap="outerHTML"
        hx-disable-elt="this">{{.Label}}</button>
`))

// ErrLiveButtonPostPathRequired is returned by LiveButton when opts.PostPath
// is empty. Exposed as a sentinel so callers can errors.Is on the value
// in unit tests / wiring code.
var ErrLiveButtonPostPathRequired = errors.New("aipanel.LiveButton: PostPath is required")

// LiveButton writes the live regenerate button HTML to w. PostPath is
// required; Label defaults to "Regenerar".
//
// When w is an http.ResponseWriter, LiveButton sets Content-Type to
// text/html; charset=utf-8 if the caller has not already done so. It does
// NOT set status — the caller owns the HTTP response code.
func LiveButton(w io.Writer, opts LiveButtonOptions) error {
	if opts.PostPath == "" {
		return ErrLiveButtonPostPathRequired
	}
	label := opts.Label
	if label == "" {
		label = "Regenerar"
	}

	if rw, ok := w.(http.ResponseWriter); ok {
		if rw.Header().Get("Content-Type") == "" {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
	}

	return liveBtnTpl.Execute(w, liveBtnData{
		PostPath: template.URL(opts.PostPath),
		Label:    label,
	})
}

// StylesheetLink writes a `<link rel="stylesheet">` tag pointing at the
// supplied href. Pass "" to use DefaultStylesheetHref. Callers include this
// in the <head> of any page that hosts the AI panel so the cooldown
// animation has the CSS it needs.
func StylesheetLink(w io.Writer, href string) error {
	if href == "" {
		href = DefaultStylesheetHref
	}
	// Use html/template URL context so href is escaped per attribute rules.
	tpl := template.Must(template.New("aipanel-css").Parse(
		`<link rel="stylesheet" href="{{.}}">`))
	if rw, ok := w.(http.ResponseWriter); ok {
		if rw.Header().Get("Content-Type") == "" {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
	}
	if err := tpl.Execute(w, template.URL(href)); err != nil {
		return fmt.Errorf("aipanel.StylesheetLink: %w", err)
	}
	return nil
}
