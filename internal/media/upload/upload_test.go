package upload

import (
	"context"
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"
)

// fakeDecoder lets tests pin exactly what DecodeConfig and Decode return,
// independent of any real codec. That's the only way to exercise the
// "header lies / decode returns a different format / encoder fails"
// branches deterministically.
type fakeDecoder struct {
	cfg     image.Config
	cfgFmt  Format
	cfgErr  error
	img     image.Image
	imgFmt  Format
	imgErr  error
	cfgHits int
	imgHits int
}

func (d *fakeDecoder) DecodeConfig(_ []byte) (image.Config, Format, error) {
	d.cfgHits++
	return d.cfg, d.cfgFmt, d.cfgErr
}

func (d *fakeDecoder) Decode(_ []byte) (image.Image, Format, error) {
	d.imgHits++
	return d.img, d.imgFmt, d.imgErr
}

type fakeReEncoder struct {
	out    []byte
	outFmt Format
	err    error
	hits   int
}

func (e *fakeReEncoder) ReEncode(_ image.Image, _ Format) ([]byte, Format, error) {
	e.hits++
	return e.out, e.outFmt, e.err
}

// pngHeader is the canonical 8-byte PNG signature plus enough following
// bytes for Sniff to consider the input "long enough" (≥12 bytes).
var pngHeader = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, // length-of-IHDR
}

var jpegHeader = []byte{
	0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01,
}

var webpHeader = []byte{
	'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P',
}

var pdfHeader = []byte("%PDF-1.4\n%xxxx\n")

// blankImage is a 1×1 RGBA image used wherever a real image.Image is
// needed for the type system but the bytes don't matter.
func blankImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	return img
}

// ----- Sniff -----------------------------------------------------------

func TestSniff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      []byte
		want    Format
		wantErr error
	}{
		{"png", pngHeader, FormatPNG, nil},
		{"jpeg", jpegHeader, FormatJPEG, nil},
		{"webp", webpHeader, FormatWEBP, nil},
		{"pdf", pdfHeader, FormatPDF, nil},
		{"too-short", []byte("RIFF"), "", ErrUnknownFormat},
		{"empty", nil, "", ErrUnknownFormat},
		{"svg-xml", []byte(`<?xml version="1.0"?><svg><script>alert(1)</script></svg>`), "", ErrUnknownFormat},
		{"svg-tag", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`), "", ErrUnknownFormat},
		{"html", []byte("<!DOCTYPE html><html><body>xss</body></html>"), "", ErrUnknownFormat},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00\x00"), "", ErrUnknownFormat},
		{"riff-but-not-webp", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), "", ErrUnknownFormat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sniff(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("format = %q, want %q", got, tc.want)
			}
		})
	}
}

// ----- Process: success path ------------------------------------------

func TestProcess_PNGSuccess(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{
		cfg:    image.Config{Width: 256, Height: 128, ColorModel: color.RGBAModel},
		cfgFmt: FormatPNG,
		img:    image.NewRGBA(image.Rect(0, 0, 256, 128)),
		imgFmt: FormatPNG,
	}
	enc := &fakeReEncoder{out: []byte("re-encoded-png"), outFmt: FormatPNG}

	res, err := Process(context.Background(), pngHeader, Policy{
		Allowed:   []Format{FormatPNG, FormatJPEG},
		MaxBytes:  1 << 20,
		MaxWidth:  1024,
		MaxHeight: 1024,
	}, dec, enc)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	if res.Format != FormatPNG {
		t.Errorf("Format = %q, want %q", res.Format, FormatPNG)
	}
	if res.Width != 256 || res.Height != 128 {
		t.Errorf("dims = %dx%d, want 256x128", res.Width, res.Height)
	}
	if string(res.Bytes) != "re-encoded-png" {
		t.Errorf("Bytes = %q, want %q", res.Bytes, "re-encoded-png")
	}
	if res.Hash == "" || len(res.Hash) != 64 {
		t.Errorf("Hash = %q (len %d), want 64-char hex", res.Hash, len(res.Hash))
	}
	if dec.cfgHits != 1 {
		t.Errorf("DecodeConfig hits = %d, want 1", dec.cfgHits)
	}
	if dec.imgHits != 1 {
		t.Errorf("Decode hits = %d, want 1", dec.imgHits)
	}
	if enc.hits != 1 {
		t.Errorf("ReEncode hits = %d, want 1", enc.hits)
	}
}

func TestProcess_DeterministicHash(t *testing.T) {
	t.Parallel()
	mkDec := func() *fakeDecoder {
		return &fakeDecoder{
			cfg: image.Config{Width: 64, Height: 64}, cfgFmt: FormatPNG,
			img: blankImage(), imgFmt: FormatPNG,
		}
	}
	enc := &fakeReEncoder{out: []byte("stable-bytes"), outFmt: FormatPNG}
	policy := Policy{Allowed: []Format{FormatPNG}}

	r1, err := Process(context.Background(), pngHeader, policy, mkDec(), enc)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := Process(context.Background(), pngHeader, policy, mkDec(), enc)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if r1.Hash != r2.Hash {
		t.Fatalf("hash drift: %s vs %s", r1.Hash, r2.Hash)
	}
}

func TestProcess_PDFBypassesReEncode(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{} // must not be called
	enc := &fakeReEncoder{}

	res, err := Process(context.Background(), pdfHeader, Policy{
		Allowed:  []Format{FormatPDF, FormatPNG},
		MaxBytes: 1 << 20,
	}, dec, enc)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Format != FormatPDF {
		t.Errorf("Format = %q, want %q", res.Format, FormatPDF)
	}
	if string(res.Bytes) != string(pdfHeader) {
		t.Errorf("Bytes = %q, want raw input", res.Bytes)
	}
	if dec.cfgHits != 0 || dec.imgHits != 0 {
		t.Errorf("decoder was called for PDF: cfg=%d img=%d", dec.cfgHits, dec.imgHits)
	}
	if enc.hits != 0 {
		t.Errorf("encoder was called for PDF: hits=%d", enc.hits)
	}
	// Mutating the returned bytes must not affect the caller's input.
	res.Bytes[0] = 0
	if pdfHeader[0] == 0 {
		t.Fatal("Process mutated caller's input slice")
	}
}

// ----- Process: failure modes -----------------------------------------

func TestProcess_RejectsEmpty(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), nil, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestProcess_RejectsTooLarge(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{
		Allowed:  []Format{FormatPNG},
		MaxBytes: 5,
	}, dec, enc)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestProcess_RejectsUnknownFormat(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{}
	enc := &fakeReEncoder{}
	// SVG with <script> is the canonical attack input from the acceptance
	// criteria; Sniff returns ErrUnknownFormat.
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	_, err := Process(context.Background(), svg, Policy{Allowed: []Format{FormatPNG, FormatJPEG, FormatWEBP, FormatPDF}}, dec, enc)
	if !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("err = %v, want ErrUnknownFormat", err)
	}
}

func TestProcess_RejectsContentTypeSpoofing(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{}
	enc := &fakeReEncoder{}
	// Body starts with `<?xml`, regardless of any client-claimed
	// Content-Type. Sniff sees no match → ErrUnknownFormat → handler
	// returns 415.
	xmlBody := []byte(`<?xml version="1.0" encoding="UTF-8"?><foo/>`)
	_, err := Process(context.Background(), xmlBody, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("err = %v, want ErrUnknownFormat", err)
	}
}

func TestProcess_RejectsFormatNotAllowed(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatJPEG}}, dec, enc)
	if !errors.Is(err, ErrFormatNotAllowed) {
		t.Fatalf("err = %v, want ErrFormatNotAllowed", err)
	}
}

func TestProcess_RejectsDecompressionBomb(t *testing.T) {
	t.Parallel()
	// Header lies: 5000 x 5000 = 25 Mpx > 16 Mpx default cap. Decode is
	// never called (the bomb cap is the whole point).
	dec := &fakeDecoder{
		cfg:    image.Config{Width: 5000, Height: 5000},
		cfgFmt: FormatPNG,
	}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrDecompressionBomb) {
		t.Fatalf("err = %v, want ErrDecompressionBomb", err)
	}
	if dec.imgHits != 0 {
		t.Fatalf("Decode was called even though bomb cap fired (hits=%d)", dec.imgHits)
	}
	if enc.hits != 0 {
		t.Fatalf("ReEncode was called for bomb input (hits=%d)", enc.hits)
	}
}

func TestProcess_DecompressionBombCustomCap(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{
		cfg:    image.Config{Width: 200, Height: 200}, // 40 000 px
		cfgFmt: FormatPNG,
	}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{
		Allowed:   []Format{FormatPNG},
		MaxPixels: 1000, // tiny cap so 40 000 trips it
	}, dec, enc)
	if !errors.Is(err, ErrDecompressionBomb) {
		t.Fatalf("err = %v, want ErrDecompressionBomb (custom cap)", err)
	}
}

func TestProcess_RejectsDimensionsExceeded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		w, h      int
		policyW   int
		policyH   int
		wantField string
	}{
		{"width-over", 2000, 100, 1024, 1024, "width"},
		{"height-over", 100, 2000, 1024, 1024, "height"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := &fakeDecoder{
				cfg: image.Config{Width: tc.w, Height: tc.h}, cfgFmt: FormatPNG,
			}
			enc := &fakeReEncoder{}
			_, err := Process(context.Background(), pngHeader, Policy{
				Allowed:   []Format{FormatPNG},
				MaxWidth:  tc.policyW,
				MaxHeight: tc.policyH,
			}, dec, enc)
			if !errors.Is(err, ErrDimensionsExceeded) {
				t.Fatalf("err = %v, want ErrDimensionsExceeded", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("err %q does not mention %q", err, tc.wantField)
			}
		})
	}
}

func TestProcess_RejectsHeaderFormatMismatch(t *testing.T) {
	t.Parallel()
	// Magic bytes say PNG but DecodeConfig disagrees — could be a
	// truncated/polyglot file. Reject at config stage.
	dec := &fakeDecoder{
		cfg:    image.Config{Width: 10, Height: 10},
		cfgFmt: FormatJPEG, // disagrees
	}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrContentTypeMismatch) {
		t.Fatalf("err = %v, want ErrContentTypeMismatch", err)
	}
}

func TestProcess_RejectsDecodedFormatMismatch(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{
		cfg:    image.Config{Width: 10, Height: 10},
		cfgFmt: FormatPNG,
		img:    blankImage(),
		imgFmt: FormatJPEG, // disagrees
	}
	enc := &fakeReEncoder{out: []byte("x"), outFmt: FormatPNG}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrContentTypeMismatch) {
		t.Fatalf("err = %v, want ErrContentTypeMismatch", err)
	}
}

func TestProcess_RejectsZeroDimensions(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{cfg: image.Config{Width: 0, Height: 0}, cfgFmt: FormatPNG}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrDecodeFailed) {
		t.Fatalf("err = %v, want ErrDecodeFailed", err)
	}
}

func TestProcess_DecodeConfigError(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{cfgErr: errors.New("crc mismatch")}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrDecodeFailed) {
		t.Fatalf("err = %v, want ErrDecodeFailed", err)
	}
}

func TestProcess_DecodeError(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{
		cfg: image.Config{Width: 10, Height: 10}, cfgFmt: FormatPNG,
		imgErr: errors.New("idat truncated"),
	}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrDecodeFailed) {
		t.Fatalf("err = %v, want ErrDecodeFailed", err)
	}
}

func TestProcess_ReEncodeError(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{
		cfg: image.Config{Width: 10, Height: 10}, cfgFmt: FormatPNG,
		img: blankImage(), imgFmt: FormatPNG,
	}
	enc := &fakeReEncoder{err: errors.New("encoder boom")}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrReEncodeFailed) {
		t.Fatalf("err = %v, want ErrReEncodeFailed", err)
	}
}

func TestProcess_NilDecoderRejected(t *testing.T) {
	t.Parallel()
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, nil, &fakeReEncoder{})
	if !errors.Is(err, ErrNilDecoder) {
		t.Fatalf("err = %v, want ErrNilDecoder", err)
	}
}

func TestProcess_NilReEncoderRejected(t *testing.T) {
	t.Parallel()
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, &fakeDecoder{}, nil)
	if !errors.Is(err, ErrNilReEncoder) {
		t.Fatalf("err = %v, want ErrNilReEncoder", err)
	}
}

func TestProcess_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Process(ctx, pngHeader, Policy{Allowed: []Format{FormatPNG}}, &fakeDecoder{}, &fakeReEncoder{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestProcess_DefaultMaxPixelsApplied(t *testing.T) {
	t.Parallel()
	// 4097x4097 = 16 785 409 px > 16 777 216 default cap.
	dec := &fakeDecoder{
		cfg: image.Config{Width: 4097, Height: 4097}, cfgFmt: FormatPNG,
	}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: []Format{FormatPNG}}, dec, enc)
	if !errors.Is(err, ErrDecompressionBomb) {
		t.Fatalf("err = %v, want ErrDecompressionBomb (default cap)", err)
	}
}

// Sanity: an empty Allowed list rejects everything (deny-by-default).
func TestPolicy_AllowsDenyByDefault(t *testing.T) {
	t.Parallel()
	dec := &fakeDecoder{}
	enc := &fakeReEncoder{}
	_, err := Process(context.Background(), pngHeader, Policy{Allowed: nil}, dec, enc)
	if !errors.Is(err, ErrFormatNotAllowed) {
		t.Fatalf("err = %v, want ErrFormatNotAllowed (empty Allowed = deny all)", err)
	}
}
