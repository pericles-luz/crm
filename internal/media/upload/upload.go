// Package upload is the pure (no I/O, no HTTP, no storage) core of the
// SIN-62246 media-upload pipeline. See docs/adr/0080-uploads.md for the
// design.
//
// Process orchestrates four independent defenses against the four classes
// of upload abuse the CRM faces (SVG-XSS, polyglot files, decompression
// bombs, Content-Type spoofing):
//
//  1. Magic-byte whitelist — Sniff identifies the format by looking at the
//     first 12 bytes. The declared Content-Type is never trusted.
//  2. Format whitelist per endpoint — Policy.Allowed enumerates exactly
//     which formats this endpoint accepts.
//  3. Decompression-bomb cap — Decoder.DecodeConfig reads the header only;
//     Process refuses to call full Decode if width*height exceeds
//     Policy.MaxPixels (default 16 Mpx).
//  4. Mandatory re-encode — for image formats, Decode + ReEncode strips
//     EXIF, ancillary chunks, ICC profiles, and any post-IEND trailers.
//     PDF bypasses re-encode (handed off to a separate scanner per
//     SIN-62228) but still passes the magic-byte and size gates.
//
// The hash returned in Result is computed over the *re-encoded* bytes (or
// the raw bytes for PDF), so callers can dedupe by content_hash and trust
// that the hash represents what actually goes to storage.
package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
)

// Format is the closed set of formats this module knows how to gate on.
type Format string

// Known formats. Anything else is ErrUnknownFormat at sniff time.
const (
	FormatPNG  Format = "png"
	FormatJPEG Format = "jpeg"
	FormatWEBP Format = "webp"
	FormatPDF  Format = "pdf"
)

// DefaultMaxPixels is the decompression-bomb cap used when Policy.MaxPixels
// is zero. 16 Mpx (4096*4096) covers every realistic legitimate upload and
// caps memory at ~64 MiB for 32-bit RGBA decoding.
const DefaultMaxPixels = 16 << 20

// Policy bundles every upload limit a caller (handler) wants to enforce.
// Allowed is required; the others are optional and disabled when zero.
type Policy struct {
	Allowed   []Format
	MaxBytes  int64
	MaxWidth  int
	MaxHeight int
	MaxPixels int
}

func (p Policy) allows(f Format) bool {
	for _, a := range p.Allowed {
		if a == f {
			return true
		}
	}
	return false
}

// Result is what a successful Process call returns. Bytes is the
// re-encoded payload (or the raw payload for PDF). Hash is SHA-256 of
// Bytes, hex-encoded, and is the value to write to media.content_hash.
type Result struct {
	Hash   string
	Format Format
	Width  int
	Height int
	Bytes  []byte
}

// Sentinel errors. All callers (handler, tests) should compare via
// errors.Is so they can distinguish 4xx categories cleanly.
var (
	ErrEmpty               = errors.New("upload: empty payload")
	ErrTooLarge            = errors.New("upload: payload exceeds max bytes")
	ErrUnknownFormat       = errors.New("upload: unknown or unsupported format")
	ErrFormatNotAllowed    = errors.New("upload: format not allowed by policy")
	ErrContentTypeMismatch = errors.New("upload: format header does not match magic bytes")
	ErrDimensionsExceeded  = errors.New("upload: image dimensions exceed policy max")
	ErrDecompressionBomb   = errors.New("upload: decompression bomb (pixel count exceeds cap)")
	ErrDecodeFailed        = errors.New("upload: decode failed")
	ErrReEncodeFailed      = errors.New("upload: re-encode failed")
	ErrNilDecoder          = errors.New("upload: nil decoder")
	ErrNilReEncoder        = errors.New("upload: nil re-encoder")
)

// Decoder is a port: raw bytes → image. Two methods so callers can read
// the header (DecodeConfig — small, allocation-free) before paying for
// the framebuffer (Decode). The decompression-bomb cap is enforced
// between them.
type Decoder interface {
	DecodeConfig(raw []byte) (image.Config, Format, error)
	Decode(raw []byte) (image.Image, Format, error)
}

// ReEncoder is a port: decoded image → fresh bytes. The output Format
// MAY differ from the input (e.g. WEBP → PNG, since the stdlib has no
// WEBP encoder); callers must use the returned Format when persisting.
type ReEncoder interface {
	ReEncode(img image.Image, in Format) ([]byte, Format, error)
}

// pngMagic, jpegMagic, webpRiff, webpTag, pdfMagic are the byte signatures
// Sniff matches against. Defined as vars (not consts) only because Go
// doesn't allow byte-slice constants.
var (
	pngMagic  = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	webpRiff  = []byte("RIFF")
	webpTag   = []byte("WEBP")
	pdfMagic  = []byte("%PDF-")
)

// Sniff inspects only the leading bytes — it never allocates a decoder.
// It returns ErrUnknownFormat for SVG, HTML, plain text, BMP, GIF, TIFF,
// and anything else not on the closed list above. This is the single
// source of truth for "what format is this really?"; the declared
// Content-Type from the upload is intentionally ignored.
func Sniff(raw []byte) (Format, error) {
	if len(raw) < 12 {
		return "", ErrUnknownFormat
	}
	switch {
	case bytes.HasPrefix(raw, pngMagic):
		return FormatPNG, nil
	case bytes.HasPrefix(raw, jpegMagic):
		return FormatJPEG, nil
	case bytes.Equal(raw[0:4], webpRiff) && bytes.Equal(raw[8:12], webpTag):
		return FormatWEBP, nil
	case bytes.HasPrefix(raw, pdfMagic):
		return FormatPDF, nil
	}
	return "", ErrUnknownFormat
}

// Process is the entry point. It enforces, in order:
//
//  1. ctx not cancelled.
//  2. raw is non-empty and within Policy.MaxBytes.
//  3. magic-byte sniff → format known.
//  4. format ∈ Policy.Allowed.
//  5. (PDF only) hash raw bytes and return; PDFs bypass image-pipeline.
//  6. DecodeConfig succeeds and reports the same format the magic bytes did.
//  7. width*height ≤ MaxPixels (default 16 Mpx).
//  8. width ≤ MaxWidth (if set) and height ≤ MaxHeight (if set).
//  9. Decode succeeds and reports the same format as DecodeConfig.
//
// 10. ReEncode succeeds.
// 11. Hash the re-encoded bytes; return Result.
//
// Every failure surfaces a sentinel error wrapped with %w so callers can
// errors.Is against ErrDecompressionBomb, ErrUnknownFormat, etc.
func Process(ctx context.Context, raw []byte, policy Policy, dec Decoder, enc ReEncoder) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if dec == nil {
		return Result{}, ErrNilDecoder
	}
	if enc == nil {
		return Result{}, ErrNilReEncoder
	}
	if len(raw) == 0 {
		return Result{}, ErrEmpty
	}
	if policy.MaxBytes > 0 && int64(len(raw)) > policy.MaxBytes {
		return Result{}, fmt.Errorf("%w: %d > %d", ErrTooLarge, len(raw), policy.MaxBytes)
	}

	sniffed, err := Sniff(raw)
	if err != nil {
		return Result{}, err
	}
	if !policy.allows(sniffed) {
		return Result{}, fmt.Errorf("%w: %s", ErrFormatNotAllowed, sniffed)
	}

	if sniffed == FormatPDF {
		// Re-encode is impossible/dangerous for PDF with stdlib. Hand off
		// to the malware scanner (SIN-62228); this module's only contract
		// for PDFs is "magic-byte gate + size gate + content-addressed".
		sum := sha256.Sum256(raw)
		out := append([]byte(nil), raw...)
		return Result{
			Hash:   hex.EncodeToString(sum[:]),
			Format: FormatPDF,
			Bytes:  out,
		}, nil
	}

	maxPixels := policy.MaxPixels
	if maxPixels <= 0 {
		maxPixels = DefaultMaxPixels
	}

	cfg, cfgFmt, err := dec.DecodeConfig(raw)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}
	if cfgFmt != sniffed {
		return Result{}, fmt.Errorf("%w: magic=%s header=%s", ErrContentTypeMismatch, sniffed, cfgFmt)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Result{}, fmt.Errorf("%w: invalid dimensions w=%d h=%d", ErrDecodeFailed, cfg.Width, cfg.Height)
	}
	// int64 multiplication so we don't wrap on a 32-bit-int build for a
	// 50000x50000 attack header.
	if int64(cfg.Width)*int64(cfg.Height) > int64(maxPixels) {
		return Result{}, fmt.Errorf("%w: %dx%d > %d", ErrDecompressionBomb, cfg.Width, cfg.Height, maxPixels)
	}
	if policy.MaxWidth > 0 && cfg.Width > policy.MaxWidth {
		return Result{}, fmt.Errorf("%w: width %d > %d", ErrDimensionsExceeded, cfg.Width, policy.MaxWidth)
	}
	if policy.MaxHeight > 0 && cfg.Height > policy.MaxHeight {
		return Result{}, fmt.Errorf("%w: height %d > %d", ErrDimensionsExceeded, cfg.Height, policy.MaxHeight)
	}

	img, decFmt, err := dec.Decode(raw)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}
	if decFmt != sniffed {
		return Result{}, fmt.Errorf("%w: magic=%s decoded=%s", ErrContentTypeMismatch, sniffed, decFmt)
	}

	out, outFmt, err := enc.ReEncode(img, sniffed)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrReEncodeFailed, err)
	}

	sum := sha256.Sum256(out)
	bounds := img.Bounds()
	return Result{
		Hash:   hex.EncodeToString(sum[:]),
		Format: outFmt,
		Width:  bounds.Dx(),
		Height: bounds.Dy(),
		Bytes:  out,
	}, nil
}
