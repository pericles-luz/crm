//go:build ignore

// gen_fixtures.go regenerates the SIN-62270 E2E upload test fixtures.
//
//	go run ./internal/adapter/web/upload/static/testdata/gen_fixtures.go
//
// It writes deterministic byte streams so the committed fixtures can be
// verified with a re-run; no randomness, no timestamps. See docs/e2e.md.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	dir, err := outDir()
	if err != nil {
		fail(err)
	}

	pngBytes, err := encodePNG()
	if err != nil {
		fail(err)
	}

	files := map[string][]byte{
		// Real PNG: 1x1 opaque red. Used by scenarios 2 and 4.
		"logo.png": pngBytes,
		// Real SVG (XML text). Used by scenario 1 — the magic-byte
		// gate must reject it before any XHR fires.
		"logo.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><rect width="1" height="1" fill="red"/></svg>`),
		// PNG bytes saved with .svg extension. Scenario 2 — magic
		// bytes win, the upload proceeds despite the misleading name.
		"png-as-svg.svg": pngBytes,
		// EXE bytes (DOS MZ header) saved with .png extension.
		// Scenario 3 — magic bytes reject before XHR fires.
		"exe-as-png.png": exeBytes(),
	}

	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fail(fmt.Errorf("write %s: %w", path, err))
		}
		fmt.Printf("wrote %s (%d bytes)\n", path, len(data))
	}
}

func encodePNG() ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func exeBytes() []byte {
	// Minimal DOS MZ stub — first two bytes "MZ" (4D 5A) are what the
	// magic-byte sniffer needs to fall through to "unknown" and reject.
	// Padded so file size > 0 in the file picker.
	out := make([]byte, 64)
	out[0] = 'M'
	out[1] = 'Z'
	out[2] = 0x90
	out[3] = 0x00
	for i := 4; i < len(out); i++ {
		out[i] = 0x00
	}
	return out
}

func outDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(wd, "internal", "adapter", "web", "upload", "static", "testdata")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	// Allow running with `go run ./internal/adapter/web/upload/static/testdata/gen_fixtures.go`
	// from inside the package dir as well.
	if _, err := os.Stat("logo.png"); err == nil {
		return wd, nil
	}
	return "", fmt.Errorf("run from repo root or testdata dir; tried %s", candidate)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen_fixtures:", err)
	os.Exit(1)
}
