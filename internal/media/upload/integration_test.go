package upload_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/adapters/imagecodec/stdlib"
	"github.com/pericles-luz/crm/internal/media/upload"
)

// makePNG produces a deterministic, decodable PNG of the given size.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 5), B: 0x40, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buf.Bytes()
}

// injectTEXt inserts a tEXt ancillary chunk after IHDR. The PNG must
// already start with the 8-byte signature followed by IHDR.
func injectTEXt(t *testing.T, raw []byte, key, value string) []byte {
	t.Helper()
	if len(raw) < 8+8+13+4 {
		t.Fatal("raw too short to be a real PNG")
	}
	// IHDR: sig(8) + len(4) + type(4) + data(13) + crc(4) = 33 bytes total.
	ihdrEnd := 8 + 4 + 4 + 13 + 4
	if string(raw[12:16]) != "IHDR" {
		t.Fatal("input doesn't start with IHDR — not a fresh png.Encode output")
	}

	// Build tEXt chunk: keyword \0 text. Per PNG spec, latin-1.
	data := []byte(key + "\x00" + value)
	chunk := buildPNGChunk("tEXt", data)

	out := make([]byte, 0, len(raw)+len(chunk))
	out = append(out, raw[:ihdrEnd]...)
	out = append(out, chunk...)
	out = append(out, raw[ihdrEnd:]...)
	return out
}

// buildPNGChunk assembles a single PNG chunk: length(BE) + type + data + CRC32.
func buildPNGChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	c := crc32.NewIEEE()
	_, _ = c.Write([]byte(typ))
	_, _ = c.Write(data)
	_ = binary.Write(&b, binary.BigEndian, c.Sum32())
	return b.Bytes()
}

// makeBombPNG fabricates a PNG whose IHDR claims width×height pixels but
// which contains no real IDAT. DecodeConfig succeeds (it only reads the
// header) and Process must reject before ever calling Decode.
func makeBombPNG(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 2  // RGB
	ihdr[10] = 0 // compression
	ihdr[11] = 0 // filter
	ihdr[12] = 0 // interlace

	var b bytes.Buffer
	b.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	b.Write(buildPNGChunk("IHDR", ihdr[:]))
	b.Write(buildPNGChunk("IEND", nil))
	return b.Bytes()
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ----- Acceptance: SVG → 415 ------------------------------------------

func TestProcess_RejectsSVGWithScript(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	payload := []byte(`<?xml version="1.0"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">
  <script>alert('xss')</script>
  <rect width="10" height="10" fill="red"/>
</svg>`)
	_, err := upload.Process(context.Background(), payload, upload.Policy{
		Allowed:  []upload.Format{upload.FormatPNG, upload.FormatJPEG, upload.FormatWEBP, upload.FormatPDF},
		MaxBytes: 1 << 20,
	}, codec, codec)
	if !errors.Is(err, upload.ErrUnknownFormat) {
		t.Fatalf("err = %v, want ErrUnknownFormat (handler maps to 415)", err)
	}
}

// ----- Acceptance: PNG with tEXt → re-encode strips chunk -------------

func TestProcess_PNGStripsTextChunk(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()

	src := makePNG(t, 32, 32)
	withScript := injectTEXt(t, src, "Comment", "<script>alert('xss')</script>")
	if !bytes.Contains(withScript, []byte("<script>alert")) {
		t.Fatal("test setup: tEXt injection didn't put <script> in raw bytes")
	}
	if sha256hex(src) == sha256hex(withScript) {
		t.Fatal("test setup: tEXt-injected hash equals original")
	}

	res, err := upload.Process(context.Background(), withScript, upload.Policy{
		Allowed:  []upload.Format{upload.FormatPNG},
		MaxBytes: 1 << 20,
	}, codec, codec)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// Acceptance: hash of re-encoded bytes ≠ hash of raw input.
	if res.Hash == sha256hex(withScript) {
		t.Fatal("re-encoded hash equals raw hash; tEXt chunk was not stripped")
	}
	// Re-encoded bytes must NOT contain the malicious payload.
	if bytes.Contains(res.Bytes, []byte("<script>")) {
		t.Fatal("re-encoded PNG still contains <script> from the tEXt chunk")
	}
	if res.Format != upload.FormatPNG {
		t.Fatalf("Format = %q, want png", res.Format)
	}
}

// ----- Acceptance: bomb PNG → ErrDecompressionBomb --------------------

func TestProcess_RejectsBombPNG(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	bomb := makeBombPNG(t, 100_000, 100_000) // 10 Gpx
	_, err := upload.Process(context.Background(), bomb, upload.Policy{
		Allowed:  []upload.Format{upload.FormatPNG},
		MaxBytes: 1 << 20,
	}, codec, codec)
	if !errors.Is(err, upload.ErrDecompressionBomb) {
		t.Fatalf("err = %v, want ErrDecompressionBomb", err)
	}
}

func TestProcess_RejectsBombPNGEvenWithLargePolicyCap(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	// 5000×5000 = 25 Mpx; default cap = 16 Mpx, no override → reject.
	bomb := makeBombPNG(t, 5000, 5000)
	_, err := upload.Process(context.Background(), bomb, upload.Policy{
		Allowed:  []upload.Format{upload.FormatPNG},
		MaxBytes: 1 << 20,
	}, codec, codec)
	if !errors.Is(err, upload.ErrDecompressionBomb) {
		t.Fatalf("err = %v, want ErrDecompressionBomb", err)
	}
}

// ----- Acceptance: Content-Type spoofing → 415 ------------------------

func TestProcess_RejectsXMLBodyEvenWhenClientClaimsPNG(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	// Handler would have read Content-Type: image/png; we don't trust
	// it. The body itself starts with `<?xml`, magic-byte sniff returns
	// ErrUnknownFormat, handler returns 415.
	body := []byte(`<?xml version="1.0"?><foo/>`)
	_, err := upload.Process(context.Background(), body, upload.Policy{
		Allowed:  []upload.Format{upload.FormatPNG},
		MaxBytes: 1 << 20,
	}, codec, codec)
	if !errors.Is(err, upload.ErrUnknownFormat) {
		t.Fatalf("err = %v, want ErrUnknownFormat", err)
	}
}

// ----- Acceptance: deterministic hash ---------------------------------

func TestProcess_DeterministicHashSameLogoTwice(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	src := makePNG(t, 64, 64)
	policy := upload.Policy{
		Allowed:   []upload.Format{upload.FormatPNG},
		MaxBytes:  1 << 20,
		MaxWidth:  1024,
		MaxHeight: 1024,
	}
	r1, err := upload.Process(context.Background(), src, policy, codec, codec)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := upload.Process(context.Background(), src, policy, codec, codec)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if r1.Hash != r2.Hash {
		t.Fatalf("hash drifted: %s vs %s", r1.Hash, r2.Hash)
	}
	if !bytes.Equal(r1.Bytes, r2.Bytes) {
		t.Fatal("re-encoded bytes drifted between identical inputs")
	}
}

// ----- Sanity: JPEG round-trips ---------------------------------------

func TestProcess_JPEGRoundTrip(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	src := makeJPEG(t, 64, 64)
	res, err := upload.Process(context.Background(), src, upload.Policy{
		Allowed:  []upload.Format{upload.FormatJPEG},
		MaxBytes: 1 << 20,
	}, codec, codec)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Format != upload.FormatJPEG {
		t.Fatalf("Format = %q, want jpeg", res.Format)
	}
	// A JPEG round-trip is still a JPEG.
	if !bytes.HasPrefix(res.Bytes, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatal("re-encoded bytes don't start with JPEG SOI")
	}
}

// ----- Sanity: enforcing per-endpoint policy --------------------------

func TestProcess_TenantLogoPolicy(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	// Tenant-logo policy: PNG/JPG/WEBP, max 2 MB, max 1024 × 1024.
	policy := upload.Policy{
		Allowed:   []upload.Format{upload.FormatPNG, upload.FormatJPEG, upload.FormatWEBP},
		MaxBytes:  2 << 20,
		MaxWidth:  1024,
		MaxHeight: 1024,
	}

	// PDF rejected by Allowed.
	pdf := []byte("%PDF-1.4\n%xxxx\n")
	_, err := upload.Process(context.Background(), pdf, policy, codec, codec)
	if !errors.Is(err, upload.ErrFormatNotAllowed) {
		t.Fatalf("PDF err = %v, want ErrFormatNotAllowed", err)
	}

	// 2000 × 100 PNG rejected by MaxWidth.
	wide := makePNG(t, 2000, 100)
	_, err = upload.Process(context.Background(), wide, policy, codec, codec)
	if !errors.Is(err, upload.ErrDimensionsExceeded) {
		t.Fatalf("wide PNG err = %v, want ErrDimensionsExceeded", err)
	}
}

// ----- Sanity: PDF policy on attachment endpoint --------------------------

func TestProcess_AttachmentPolicyAcceptsPDF(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	policy := upload.Policy{
		Allowed:  []upload.Format{upload.FormatPNG, upload.FormatJPEG, upload.FormatWEBP, upload.FormatPDF},
		MaxBytes: 20 << 20,
	}
	pdf := []byte("%PDF-1.4\n%xxxx\nBody bytes here as a placeholder.\n")
	res, err := upload.Process(context.Background(), pdf, policy, codec, codec)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Format != upload.FormatPDF {
		t.Fatalf("Format = %q, want pdf", res.Format)
	}
	// PDF must round-trip byte-for-byte (no re-encode).
	if !bytes.Equal(res.Bytes, pdf) {
		t.Fatal("PDF bytes mutated by Process")
	}
	// Hash must match SHA-256 of raw PDF.
	if res.Hash != sha256hex(pdf) {
		t.Fatalf("hash mismatch: got %s, want %s", res.Hash, sha256hex(pdf))
	}
}

// ----- Sanity: helper string for diagnostics --------------------------

func TestProcess_ErrorIncludesContextOnRejection(t *testing.T) {
	t.Parallel()
	codec := stdlib.New()
	bomb := makeBombPNG(t, 50000, 50000)
	_, err := upload.Process(context.Background(), bomb, upload.Policy{
		Allowed:  []upload.Format{upload.FormatPNG},
		MaxBytes: 1 << 20,
	}, codec, codec)
	if err == nil {
		t.Fatal("expected error")
	}
	// Error wrapping must surface useful diagnostic context for ops.
	if !strings.Contains(err.Error(), "50000") {
		t.Fatalf("err %q lost the bad dimension; ops will hate this", err)
	}
}
