// Package serve is the HTTP adapter that exposes tenant media through the
// cookieless `static.<primary>` domain (SIN-62257, F49). It owns:
//
//  1. The Store and Blob ports — small, accept-broad / return-narrow
//     interfaces that the storage adapter implements. The handler never
//     imports database/sql nor touches a session: Caddy strips inbound
//     Cookie headers and Set-Cookie outbound, and this package never
//     reads either.
//  2. The HTTP routes:
//     - `GET /t/{tenantID}/logo`  → resolves the active tenant logo via
//     Store.LookupLogo and streams its blob.
//     - `GET /t/{tenantID}/m/{hash}` → resolves a content-addressed blob
//     via Store.LookupHash. Path enforcement is the Store's contract:
//     it MUST return ErrNotFound when the hash exists for some other
//     tenant; the handler does not attempt 403, since cross-tenant
//     existence must not leak.
//  3. The MediaHeaders middleware that applies the static-origin
//     defense-in-depth headers on every response (nosniff, restrictive
//     CSP, Vary on Origin). Per-resource Cache-Control and
//     Content-Disposition are set in-handler since they vary by route.
//
// See ADR 0080 §7 and the SIN-62226 decisions document.
package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/upload"
)

// ErrNotFound is the sentinel returned by Store implementations when the
// requested media does not exist or — for content-addressed lookups —
// exists but belongs to a different tenant. Handlers map it to HTTP 404
// without distinguishing the two cases (see SIN-62226 decisions §7:
// cross-tenant existence must not leak via 403/404 difference).
var ErrNotFound = errors.New("media: not found")

// hashHexLen is the expected length of a hex-encoded SHA-256 digest
// (32 bytes × 2). Anything else fails validation before we ever touch
// the Store, so an attacker cannot probe storage with arbitrary strings.
const hashHexLen = 64

// Media is the metadata returned by Store. The handler never sets these
// fields — the storage adapter owns them and the values are derived
// server-side at upload time (see internal/media/upload). The
// StoragePath in particular is server-controlled and never built from
// user input (path-traversal closure, ADR 0080 §5).
type Media struct {
	TenantID    uuid.UUID
	Hash        string
	Format      upload.Format
	StoragePath string
	// Filename is the original client-supplied name, kept only for the
	// Content-Disposition header on non-image downloads. It is sanitized
	// before being written to the wire (see safeFilename).
	Filename  string
	SizeBytes int64
}

// Store is the lookup port. It does not stream the binary itself; it
// only resolves a URL path to a Media row (or ErrNotFound). Splitting
// lookup from blob streaming lets the storage adapter (Postgres + S3,
// Postgres + filesystem, etc.) compose freely.
type Store interface {
	// LookupLogo returns the active logo for tenantID, or ErrNotFound if
	// the tenant has none configured (tenant.logo_media_id NULL).
	LookupLogo(ctx context.Context, tenantID uuid.UUID) (Media, error)
	// LookupHash returns the media identified by (tenantID, hash). The
	// implementation MUST enforce the tenant match in the query and
	// return ErrNotFound on mismatch, even when the hash exists for
	// another tenant. This is the path-enforcement contract from SIN-62226 §7.
	LookupHash(ctx context.Context, tenantID uuid.UUID, hash string) (Media, error)
}

// Blob is the binary-streaming port. Implementations open a reader for
// a server-controlled storage path. The caller is responsible for
// closing the returned ReadCloser.
type Blob interface {
	Open(ctx context.Context, path string) (io.ReadCloser, error)
}

// Handler config. Both ports are required.
type Handler struct {
	store Store
	blob  Blob
}

// New builds the handler with its dependencies validated up front so
// misconfiguration fails at boot, not on the first request.
func New(store Store, blob Blob) (*Handler, error) {
	if store == nil {
		return nil, errors.New("serve: nil store")
	}
	if blob == nil {
		return nil, errors.New("serve: nil blob")
	}
	return &Handler{store: store, blob: blob}, nil
}

// Routes wires the handler into a *http.ServeMux with the two static
// routes and the MediaHeaders middleware applied. The returned
// http.Handler is what the server should mount on the static origin.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t/{tenantID}/logo", h.serveLogo)
	mux.HandleFunc("GET /t/{tenantID}/m/{hash}", h.serveContent)
	return MediaHeaders(mux)
}

// serveLogo resolves and streams the tenant logo. Failures (bad UUID,
// missing logo, blob error) produce 404 without distinguishing the
// reason on the wire.
func (h *Handler) serveLogo(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parseTenantID(r.PathValue("tenantID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	m, err := h.store.LookupLogo(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Logos are short-cached and revalidated; avatar swaps must take
	// effect within minutes without busting browsers' image caches
	// entirely (SIN-62226 decisions §7).
	w.Header().Set("Cache-Control", "private, max-age=300, must-revalidate")
	h.streamMedia(w, r, m)
}

// serveContent resolves and streams a content-addressed blob. The
// (tenantID, hash) match is enforced inside Store.LookupHash; this
// handler validates only the hex shape of the hash before touching it.
func (h *Handler) serveContent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parseTenantID(r.PathValue("tenantID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	hash := r.PathValue("hash")
	if !validHexHash(hash) {
		http.NotFound(w, r)
		return
	}
	m, err := h.store.LookupHash(r.Context(), tenantID, hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Content-addressed blobs never change for a given hash, so we can
	// cache them aggressively; private keeps shared caches (proxies)
	// from holding bytes that may be tenant-correlatable.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	h.streamMedia(w, r, m)
}

// streamMedia is the common tail: open the blob, set Content-Type
// derived strictly from the stored Format (never the request),
// optionally set Content-Disposition for non-image MIME, copy bytes.
func (h *Handler) streamMedia(w http.ResponseWriter, r *http.Request, m Media) {
	mime, ok := mimeFor(m.Format)
	if !ok {
		// An unknown stored format means the upload pipeline accepted
		// something the serve pipeline cannot mime-type — should never
		// happen, but failing closed beats serving with a sniffable
		// type.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rc, err := h.blob.Open(r.Context(), m.StoragePath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", mime)
	if m.SizeBytes > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", m.SizeBytes))
	}
	if !isImage(m.Format) {
		// Force download for any future non-image format (PDF today,
		// others later). The filename is the only client-supplied
		// string that ever crosses into a response header here, so it
		// passes through safeFilename to neutralize CRLF, quotes, and
		// non-ASCII control bytes.
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(m.Filename)+`"`)
	}

	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already on the wire; nothing actionable to do
		// beyond not panicking.
		return
	}
}

// parseTenantID validates the UUID shape before hitting the store.
// Returning a 404 (not a 400) on malformed UUIDs keeps probing
// harder — clients can't tell "no such tenant" from "syntactically
// invalid".
func parseTenantID(s string) (uuid.UUID, bool) {
	if s == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, false
	}
	if id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// validHexHash accepts only lowercase 64-char hex (SHA-256). The Store
// would also reject mismatches, but pre-filtering here keeps malformed
// requests from ever reaching the database.
func validHexHash(h string) bool {
	if len(h) != hashHexLen {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// mimeFor maps the closed Format set to MIME types. Driven from the
// stored format, never from the request, so attackers cannot trick the
// browser into sniffing HTML out of an image upload.
func mimeFor(f upload.Format) (string, bool) {
	switch f {
	case upload.FormatPNG:
		return "image/png", true
	case upload.FormatJPEG:
		return "image/jpeg", true
	case upload.FormatWEBP:
		return "image/webp", true
	case upload.FormatPDF:
		return "application/pdf", true
	}
	return "", false
}

// isImage answers "should this be inline-renderable in <img>?". Drives
// the Content-Disposition decision: images render inline, everything
// else (PDF today) is forced to attachment.
func isImage(f upload.Format) bool {
	switch f {
	case upload.FormatPNG, upload.FormatJPEG, upload.FormatWEBP:
		return true
	}
	return false
}

// safeFilename produces a quoted-printable-ascii filename safe for use
// inside `Content-Disposition: attachment; filename="..."`. It strips
// any byte outside printable 7-bit ASCII (no CR/LF, no NUL, no UTF-8
// trail bytes), and the structurally dangerous `"` and `\`. Inputs
// that contain no safe character at all fall back to "file" so the
// header is always well-formed.
func safeFilename(name string) string {
	name = strings.TrimSpace(name)
	hasSafe := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 0x20 && c < 0x7F && c != '"' && c != '\\' {
			hasSafe = true
			break
		}
	}
	if !hasSafe {
		return "file"
	}
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 0x20 && c < 0x7F && c != '"' && c != '\\' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
