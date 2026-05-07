// Package stdlib is the boring-tech adapter for the upload.Decoder /
// upload.ReEncoder ports. It uses image/png and image/jpeg from the Go
// stdlib plus golang.org/x/image/webp (a sub-repository of the Go
// project) — no third-party image library, no cgo, no shell-out.
//
// Decode strategy:
//   - PNG, JPEG decode via stdlib.
//   - WEBP decodes via golang.org/x/image/webp (decode-only; that
//     subrepo has no encoder).
//
// Re-encode strategy:
//   - PNG → PNG.
//   - JPEG → JPEG quality=90 (sweet-spot for re-encode loss vs. size).
//   - WEBP → PNG (lossless; no stdlib WEBP encoder exists). The chosen
//     output format is reported to the caller so persistence picks the
//     right extension.
//
// Re-encoding through these encoders implicitly strips:
//
//   - PNG ancillary chunks (tEXt, iTXt, zTXt, eXIf, sPLT, …) that Go's
//     png.Encode does not emit.
//   - JPEG APP1/APP2 segments (EXIF, ICC).
//   - Any post-IEND or post-EOI trailer bytes (a common polyglot trick).
//   - Arbitrary RIFF chunks in WEBP (decoded → re-encoded as PNG).
package stdlib

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"

	"github.com/pericles-luz/crm/internal/media/upload"
)

// jpegQuality is the encoder Q for JPEG re-encode. 90 is the empirical
// sweet-spot: ~10–15% larger than Q85 with no visible difference, ~30%
// smaller than Q95 with no visible difference for most photos.
const jpegQuality = 90

// Codec is both upload.Decoder and upload.ReEncoder. Stateless — share a
// single instance across handlers.
type Codec struct{}

// New returns a ready-to-use Codec.
func New() *Codec { return &Codec{} }

// Compile-time assertion: Codec implements both ports.
var (
	_ upload.Decoder   = (*Codec)(nil)
	_ upload.ReEncoder = (*Codec)(nil)
)

// DecodeConfig reads only the image header. Allocation here is the
// constant-size image.Config struct (24 bytes); the framebuffer is NOT
// allocated. This is what makes the upload pipeline's
// decompression-bomb cap effective — Process can refuse to call Decode
// based on the dimensions reported here.
func (Codec) DecodeConfig(raw []byte) (image.Config, upload.Format, error) {
	if len(raw) == 0 {
		return image.Config{}, "", errors.New("stdlib: empty input")
	}
	switch {
	case isPNG(raw):
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return image.Config{}, "", fmt.Errorf("stdlib: png header: %w", err)
		}
		return cfg, upload.FormatPNG, nil
	case isJPEG(raw):
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return image.Config{}, "", fmt.Errorf("stdlib: jpeg header: %w", err)
		}
		return cfg, upload.FormatJPEG, nil
	case isWEBP(raw):
		cfg, err := webp.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return image.Config{}, "", fmt.Errorf("stdlib: webp header: %w", err)
		}
		return cfg, upload.FormatWEBP, nil
	}
	return image.Config{}, "", fmt.Errorf("stdlib: unrecognized magic bytes")
}

// Decode reads the full image. Callers MUST have validated dimensions
// via DecodeConfig first; Decode itself does no pixel-cap check, by
// design — the policy lives in the upload module.
func (Codec) Decode(raw []byte) (image.Image, upload.Format, error) {
	if len(raw) == 0 {
		return nil, "", errors.New("stdlib: empty input")
	}
	switch {
	case isPNG(raw):
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, "", fmt.Errorf("stdlib: png decode: %w", err)
		}
		return img, upload.FormatPNG, nil
	case isJPEG(raw):
		img, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, "", fmt.Errorf("stdlib: jpeg decode: %w", err)
		}
		return img, upload.FormatJPEG, nil
	case isWEBP(raw):
		img, err := webp.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, "", fmt.Errorf("stdlib: webp decode: %w", err)
		}
		return img, upload.FormatWEBP, nil
	}
	return nil, "", fmt.Errorf("stdlib: unrecognized magic bytes")
}

// ReEncode serializes img back to bytes. WEBP input is converted to PNG
// because there is no WEBP encoder in stdlib or in x/image; the
// returned Format reflects what the caller should persist.
func (Codec) ReEncode(img image.Image, in upload.Format) ([]byte, upload.Format, error) {
	if img == nil {
		return nil, "", errors.New("stdlib: nil image")
	}
	var buf bytes.Buffer
	switch in {
	case upload.FormatPNG, upload.FormatWEBP:
		// WEBP → PNG: lossless conversion, no fidelity loss; we don't
		// have an encoder for WEBP so PNG is the safest fallback.
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("stdlib: png re-encode: %w", err)
		}
		return buf.Bytes(), upload.FormatPNG, nil
	case upload.FormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, "", fmt.Errorf("stdlib: jpeg re-encode: %w", err)
		}
		return buf.Bytes(), upload.FormatJPEG, nil
	default:
		return nil, "", fmt.Errorf("stdlib: cannot re-encode format %q", in)
	}
}

// isPNG / isJPEG / isWEBP duplicate the magic-byte sniff from upload.Sniff
// rather than calling it. Two reasons:
//   - Adapter shouldn't take a runtime dependency on Sniff's internal
//     dispatch — keeps the port boundary clean.
//   - Sniff returns ErrUnknownFormat for SVG/PDF/etc.; here we only need
//     the codec-level "which decoder do I dispatch to?".
func isPNG(b []byte) bool {
	return len(b) >= 8 &&
		b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4E && b[3] == 0x47 &&
		b[4] == 0x0D && b[5] == 0x0A && b[6] == 0x1A && b[7] == 0x0A
}

func isJPEG(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

func isWEBP(b []byte) bool {
	return len(b) >= 12 &&
		b[0] == 'R' && b[1] == 'I' && b[2] == 'F' && b[3] == 'F' &&
		b[8] == 'W' && b[9] == 'E' && b[10] == 'B' && b[11] == 'P'
}
