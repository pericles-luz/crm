package stdlib_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/pericles-luz/crm/adapters/imagecodec/stdlib"
	"github.com/pericles-luz/crm/internal/media/upload"
)

func mkPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0xAA, A: 0xFF})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return b.Bytes()
}

func mkJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return b.Bytes()
}

func TestCodec_DecodeConfig(t *testing.T) {
	t.Parallel()
	c := stdlib.New()

	t.Run("png", func(t *testing.T) {
		raw := mkPNG(t, 16, 8)
		cfg, f, err := c.DecodeConfig(raw)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if f != upload.FormatPNG {
			t.Fatalf("format = %q", f)
		}
		if cfg.Width != 16 || cfg.Height != 8 {
			t.Fatalf("dims = %dx%d", cfg.Width, cfg.Height)
		}
	})

	t.Run("jpeg", func(t *testing.T) {
		raw := mkJPEG(t, 8, 4)
		cfg, f, err := c.DecodeConfig(raw)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if f != upload.FormatJPEG {
			t.Fatalf("format = %q", f)
		}
		if cfg.Width != 8 || cfg.Height != 4 {
			t.Fatalf("dims = %dx%d", cfg.Width, cfg.Height)
		}
	})

	t.Run("empty", func(t *testing.T) {
		_, _, err := c.DecodeConfig(nil)
		if err == nil {
			t.Fatal("err = nil, want non-nil for empty input")
		}
	})

	t.Run("unrecognized", func(t *testing.T) {
		_, _, err := c.DecodeConfig([]byte("garbage data here"))
		if err == nil {
			t.Fatal("err = nil, want non-nil for garbage input")
		}
	})

	t.Run("png-truncated", func(t *testing.T) {
		raw := mkPNG(t, 4, 4)
		_, _, err := c.DecodeConfig(raw[:10]) // header sig + 2 bytes; IHDR truncated
		if err == nil {
			t.Fatal("err = nil, want non-nil for truncated PNG")
		}
	})

	t.Run("webp-malformed-dispatches", func(t *testing.T) {
		// Looks like a WEBP at the magic-byte level (RIFF…WEBP) but
		// the VP8 chunk is missing/garbage. Goal here is to prove
		// DecodeConfig dispatches into the WEBP branch (covering it)
		// rather than verify any particular error string.
		raw := []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P', 0, 0, 0, 0}
		_, _, err := c.DecodeConfig(raw)
		if err == nil {
			t.Fatal("err = nil, want non-nil for malformed WEBP")
		}
	})
}

func TestCodec_Decode(t *testing.T) {
	t.Parallel()
	c := stdlib.New()

	t.Run("png", func(t *testing.T) {
		raw := mkPNG(t, 4, 4)
		img, f, err := c.Decode(raw)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if f != upload.FormatPNG {
			t.Fatalf("format = %q", f)
		}
		if img.Bounds().Dx() != 4 || img.Bounds().Dy() != 4 {
			t.Fatalf("bounds = %v", img.Bounds())
		}
	})

	t.Run("jpeg", func(t *testing.T) {
		raw := mkJPEG(t, 4, 4)
		img, f, err := c.Decode(raw)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if f != upload.FormatJPEG {
			t.Fatalf("format = %q", f)
		}
		if img == nil {
			t.Fatal("img = nil")
		}
	})

	t.Run("empty", func(t *testing.T) {
		_, _, err := c.Decode(nil)
		if err == nil {
			t.Fatal("err = nil, want non-nil")
		}
	})

	t.Run("unrecognized", func(t *testing.T) {
		_, _, err := c.Decode([]byte("not an image at all here"))
		if err == nil {
			t.Fatal("err = nil, want non-nil")
		}
	})

	t.Run("webp-malformed-dispatches", func(t *testing.T) {
		raw := []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P', 0, 0, 0, 0}
		_, _, err := c.Decode(raw)
		if err == nil {
			t.Fatal("err = nil, want non-nil for malformed WEBP")
		}
	})
}

func TestCodec_ReEncode(t *testing.T) {
	t.Parallel()
	c := stdlib.New()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))

	t.Run("png-out-png", func(t *testing.T) {
		out, f, err := c.ReEncode(img, upload.FormatPNG)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if f != upload.FormatPNG {
			t.Fatalf("format = %q", f)
		}
		if !bytes.HasPrefix(out, []byte{0x89, 0x50, 0x4E, 0x47}) {
			t.Fatal("out does not start with PNG signature")
		}
	})

	t.Run("webp-becomes-png", func(t *testing.T) {
		out, f, err := c.ReEncode(img, upload.FormatWEBP)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if f != upload.FormatPNG {
			t.Fatalf("format = %q, want png (no stdlib WEBP encoder)", f)
		}
		if !bytes.HasPrefix(out, []byte{0x89, 0x50, 0x4E, 0x47}) {
			t.Fatal("out does not start with PNG signature")
		}
	})

	t.Run("jpeg", func(t *testing.T) {
		out, f, err := c.ReEncode(img, upload.FormatJPEG)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if f != upload.FormatJPEG {
			t.Fatalf("format = %q", f)
		}
		if !bytes.HasPrefix(out, []byte{0xFF, 0xD8, 0xFF}) {
			t.Fatal("out does not start with JPEG SOI")
		}
	})

	t.Run("nil-image", func(t *testing.T) {
		_, _, err := c.ReEncode(nil, upload.FormatPNG)
		if err == nil {
			t.Fatal("err = nil, want non-nil for nil image")
		}
	})

	t.Run("unsupported-format", func(t *testing.T) {
		_, _, err := c.ReEncode(img, upload.Format("svg"))
		if err == nil {
			t.Fatal("err = nil, want non-nil for unsupported format")
		}
	})

	t.Run("pdf-not-encodable", func(t *testing.T) {
		_, _, err := c.ReEncode(img, upload.FormatPDF)
		if err == nil {
			t.Fatal("err = nil, want non-nil; PDF must not be re-encodable as image")
		}
	})
}
