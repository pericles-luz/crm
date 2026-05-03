// Package upload renders the SIN-62258 file-upload UI: a logo form and a
// message-attachment form. Both forms ship vanilla JS that mirrors the
// magic-byte / size policy enforced server-side by internal/media/upload
// (SIN-62246, see docs/adr/0080-uploads.md).
//
// The package is web-adapter-only — it has no domain logic and no I/O
// other than rendering HTML. The handler that *receives* the upload
// lives elsewhere (a future ticket); this package only presents the
// form, the static JS that gates client-side, and the PT-BR error
// table that translates server status/error-code responses for users.
package upload

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
)

// LogoMaxBytes is the default 2 MB limit for white-label logo uploads.
// Mirrors the server-side cap in internal/media/upload Policy for
// /uploads/logo (set when the corresponding handler is wired).
const LogoMaxBytes = 2 << 20

// AttachmentMaxBytes is the default 20 MB limit for message attachments.
// Mirrors the server-side cap for /uploads/attachment.
const AttachmentMaxBytes = 20 << 20

// Kind identifies which form to render.
type Kind string

// Recognised form kinds. Anything else is rejected by Render.
const (
	KindLogo       Kind = "logo"
	KindAttachment Kind = "attachment"
)

// ErrUnknownKind is returned by Render when k is neither KindLogo nor
// KindAttachment. It exists so the http handler can map it to 404 cleanly.
var ErrUnknownKind = errors.New("upload: unknown form kind")

// FormConfig is the data passed to the HTML template. All fields are
// optional except those documented as required.
type FormConfig struct {
	// ID is the form's DOM id prefix. Required (used by aria-describedby).
	ID string
	// Label is the human-readable PT-BR label for the file input.
	Label string
	// Action is the URL the form posts to (hx-post / form action).
	Action string
	// Target is the HTMX hx-target selector for the response swap. Defaults
	// to "this" (replace the form itself).
	Target string
	// MaxBytes is the client-side size cap. 0 falls back to the kind's
	// default (LogoMaxBytes / AttachmentMaxBytes).
	MaxBytes int64
	// CSRFFieldName / CSRFToken render a hidden input for CSRF protection
	// when both are non-empty. Caller wires this to its session middleware.
	CSRFFieldName string
	CSRFToken     string
}

// templateData is the value the html/template templates render against.
// It carries FormConfig plus a few derived/already-resolved fields so
// the templates stay declarative (no logic).
type templateData struct {
	ID            string
	Label         string
	Action        string
	Target        string
	MaxBytes      int64
	MaxBytesHuman string
	CSRFFieldName string
	CSRFToken     string
}

//go:embed static/templates.html static/upload.js static/upload.css
var staticFS embed.FS

var formTemplates = template.Must(template.ParseFS(staticFS, "static/templates.html"))

// Render writes the requested form to w. It returns ErrUnknownKind if k
// is not a supported form kind, and any error from template execution.
//
// All defaults (MaxBytes, Target, Label, ID, Action) are applied here so
// callers can pass an empty FormConfig and still get a working form. ID
// is the only field with no sensible default; an empty ID becomes
// "sin-upload-<kind>" to keep aria-describedby valid.
func Render(w io.Writer, k Kind, cfg FormConfig) error {
	d, name, err := resolve(k, cfg)
	if err != nil {
		return err
	}
	return formTemplates.ExecuteTemplate(w, name, d)
}

func resolve(k Kind, cfg FormConfig) (templateData, string, error) {
	var (
		name        string
		defaultMax  int64
		defaultLbl  string
		defaultID   string
		defaultPath string
	)
	switch k {
	case KindLogo:
		name = "logoForm"
		defaultMax = LogoMaxBytes
		defaultLbl = "Logo da empresa (PNG, JPG ou WEBP)"
		defaultID = "sin-upload-logo"
		defaultPath = "/uploads/logo"
	case KindAttachment:
		name = "attachmentForm"
		defaultMax = AttachmentMaxBytes
		defaultLbl = "Anexo (PNG, JPG, WEBP ou PDF)"
		defaultID = "sin-upload-attachment"
		defaultPath = "/uploads/attachment"
	default:
		return templateData{}, "", fmt.Errorf("%w: %q", ErrUnknownKind, k)
	}
	d := templateData{
		ID:            firstNonEmpty(cfg.ID, defaultID),
		Label:         firstNonEmpty(cfg.Label, defaultLbl),
		Action:        firstNonEmpty(cfg.Action, defaultPath),
		Target:        firstNonEmpty(cfg.Target, "this"),
		MaxBytes:      pickInt64(cfg.MaxBytes, defaultMax),
		CSRFFieldName: cfg.CSRFFieldName,
		CSRFToken:     cfg.CSRFToken,
	}
	d.MaxBytesHuman = HumanBytes(d.MaxBytes)
	return d, name, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func pickInt64(a, b int64) int64 {
	if a > 0 {
		return a
	}
	return b
}

// HumanBytes renders a byte budget in PT-BR-flavoured short form: it
// rounds to one decimal MB above 1 MB, falls back to KB below. We mirror
// the JS formatBytes so PT-BR strings on both sides agree.
func HumanBytes(n int64) string {
	if n <= 0 {
		return "0KB"
	}
	const mb = 1024 * 1024
	if n >= mb {
		mbVal := float64(n) / float64(mb)
		// Round to one decimal, then strip trailing ".0".
		rounded := round1(mbVal)
		if rounded == float64(int64(rounded)) {
			return strconv.FormatInt(int64(rounded), 10) + "MB"
		}
		return strconv.FormatFloat(rounded, 'f', 1, 64) + "MB"
	}
	kb := n / 1024
	if kb < 1 {
		kb = 1
	}
	return strconv.FormatInt(kb, 10) + "KB"
}

func round1(v float64) float64 {
	// Standard half-away-from-zero rounding to 1 decimal place.
	if v >= 0 {
		return float64(int64(v*10+0.5)) / 10
	}
	return float64(int64(v*10-0.5)) / 10
}

// StaticFS exposes the embedded static assets (upload.js, upload.css,
// templates.html) so callers can mount them with http.FileServer at
// whatever path they prefer.
//
// Returned filesystem is rooted at the package's "static/" directory:
// fs.Open("upload.js") works directly.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Embedded FS guarantees this; panic surfaces a build-time bug.
		panic(fmt.Sprintf("upload: static sub-fs: %v", err))
	}
	return sub
}

// StaticHandler returns an http.Handler that serves the embedded static
// assets at /<asset>. Use it under a prefix (e.g. "/static/upload/")
// with http.StripPrefix.
func StaticHandler() http.Handler {
	return http.FileServerFS(StaticFS())
}
