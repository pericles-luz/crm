// Package branding defines the per-tenant visual identity domain: the
// extracted colour Palette, its CSS-variable projection ThemeTokens,
// and the PaletteExtractor port that derives a Palette from a tenant
// logo.
//
// The package is deliberately storage- and transport-agnostic: it
// imports nothing beyond the Go standard library, so any caller — a
// usecase, an HTTP handler, a worker — can wire it without dragging
// imaging libraries along. Concrete extraction is the responsibility
// of an adapter under internal/adapter/branding/{mediancut,…},
// selected at composition time. See ADR 0060 for the rationale and
// the WCAG AA contrast policy this port enforces.
//
// This file is a port stub: it defines types and the interface only.
// The reference adapter (median-cut via cenkalti/dominantcolor) ships
// in SIN-63076.
package branding

import (
	"context"
	"errors"
	"io"
)

// RGB is an 8-bit-per-channel sRGB colour. Adapters returning a
// Palette MUST emit RGB values, not Go's image/color.Color, so the
// domain stays independent of the imaging stack.
type RGB struct {
	R, G, B uint8
}

// PaletteSource records how the Palette was produced. Producers use
// it to surface UI affordances (e.g. "extracted from your logo" vs.
// "we couldn't derive a unique brand colour — adjust manually").
type PaletteSource uint8

const (
	// PaletteSourceUnknown is the zero value; never emitted by a
	// well-behaved adapter.
	PaletteSourceUnknown PaletteSource = iota
	// PaletteSourceExtracted means the Primary/Secondary/Accent slots
	// came from the logo without falling back to the neutral default.
	PaletteSourceExtracted
	// PaletteSourceFallback means the extractor could not derive a
	// palette satisfying WCAG AA from the logo and substituted the
	// deterministic neutral pair documented in ADR 0060.
	PaletteSourceFallback
	// PaletteSourceManual is reserved for future producer paths where
	// a human-edited palette bypasses extraction entirely. Adapters
	// MUST NOT emit it.
	PaletteSourceManual
)

// Palette is the canonical five-slot tenant palette emitted by a
// PaletteExtractor. Foreground/Background and the implicit
// text-on-Primary pair are guaranteed by the adapter to meet WCAG AA
// (contrast ratio ≥ 4.5:1 for normal text); see ADR 0060.
//
// Producers MUST NOT recompute contrast at render time.
type Palette struct {
	Primary    RGB
	Secondary  RGB
	Accent     RGB
	Foreground RGB
	Background RGB

	// TextOnPrimary is the colour producers should use for text
	// rendered on a Primary plate (CTA labels, badges). It is one of
	// the two deterministic candidates documented in ADR 0060 and is
	// guaranteed to reach CR ≥ 4.5 against Primary.
	TextOnPrimary RGB

	Source PaletteSource
}

// Hint carries advisory metadata about the source bytes. Adapters MAY
// use ContentType to short-circuit decoder selection, but MUST still
// validate the actual bytes (magic-byte sniff) — upload-time format
// validation is documented in ADR 0080 and is not duplicated here.
//
// MaxBytes is the byte budget the adapter is allowed to read from
// src. A value ≤ 0 means "use the adapter's default" (typically
// 2 MiB, matching ADR 0080's tenant-logo cap).
type Hint struct {
	ContentType string
	MaxBytes    int64
}

// PaletteExtractor is the domain port for deriving a Palette from a
// tenant logo.
//
// Implementations MUST:
//   - honour ctx (deadline, cancellation) on all I/O,
//   - read at most Hint.MaxBytes (or the default) bytes from src,
//   - return an error wrapping ErrUnsupportedFormat for inputs whose
//     decoded format the adapter does not handle (the upload layer
//     already rejects SVG and unknown types — this is defence in
//     depth),
//   - return an error wrapping ErrTooLarge for inputs that exceed the
//     byte or pixel budget,
//   - return an error wrapping ErrUnavailable for transient failures
//     (out-of-memory while decoding, dependency crash) — producers
//     fall back to the default neutral palette on this sentinel,
//   - return an error wrapping ErrInvalidImage for unrecoverable
//     decode failures (truncated bytes, declared format mismatch),
//   - return a Palette satisfying the WCAG AA contrast guarantees
//     described in ADR 0060 on success.
//
// Implementations MUST be deterministic: the same input bytes always
// yield the same Palette. This is required so the "revert to default
// palette" UX is well-defined and so unit tests can pin exact RGB
// triples.
type PaletteExtractor interface {
	Extract(ctx context.Context, src io.Reader, hint Hint) (Palette, error)
}

// Sentinels for caller-side classification. Wrap with
// fmt.Errorf("…: %w", branding.ErrFoo) so errors.Is keeps working
// through the chain.
var (
	// ErrUnsupportedFormat marks inputs whose format the adapter
	// cannot decode (e.g. SVG, TIFF). The upload layer (ADR 0080)
	// already filters formats; this sentinel exists for defence in
	// depth and for adapters with stricter format subsets.
	ErrUnsupportedFormat = errors.New("branding: unsupported logo format")
	// ErrTooLarge marks inputs that exceed Hint.MaxBytes or the
	// adapter's declared pixel ceiling. Producers SHOULD surface a
	// 413-equivalent UI message and NOT retry.
	ErrTooLarge = errors.New("branding: logo exceeds size budget")
	// ErrInvalidImage marks unrecoverable decode failures — truncated
	// bytes, content-type mismatch, malformed chunks. Treat as a
	// permanent error; do not retry.
	ErrInvalidImage = errors.New("branding: logo failed to decode")
	// ErrUnavailable marks transient failures inside the adapter
	// (memory pressure, dependency crash). Producers fall back to the
	// neutral default palette and record metric outcome=error.
	ErrUnavailable = errors.New("branding: palette extractor unavailable")
)
