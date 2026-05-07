package upload

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStoragePath_HappyPath(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	when := time.Date(2026, 5, 2, 14, 0, 0, 0, time.UTC)
	r := Result{Hash: "abcdef0123456789", Format: FormatPNG}

	got, err := StoragePath(tenant, when, r)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := "media/11111111-2222-3333-4444-555555555555/2026-05/abcdef0123456789.png"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestStoragePath_MonthBoundary(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	// 23:59 UTC on Jan 31 is still January.
	when := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	r := Result{Hash: "deadbeef", Format: FormatJPEG}

	got, err := StoragePath(tenant, when, r)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(got, "/2026-01/") {
		t.Fatalf("path = %q does not contain /2026-01/", got)
	}
	if !strings.HasSuffix(got, ".jpg") {
		t.Fatalf("path = %q, want .jpg suffix for FormatJPEG", got)
	}
}

func TestStoragePath_UTCNormalization(t *testing.T) {
	t.Parallel()
	// 2026-01-31 22:30 in UTC-3 is 2026-02-01 01:30 UTC; the path must
	// use UTC ("yyyy-mm" partition is logical, not local) so the bucket
	// is February even though the wall clock locally still says January.
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	loc := time.FixedZone("brt", -3*60*60)
	local := time.Date(2026, 1, 31, 22, 30, 0, 0, loc)
	r := Result{Hash: "deadbeef", Format: FormatPNG}

	got, err := StoragePath(tenant, local, r)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(got, "/2026-02/") {
		t.Fatalf("path = %q expected UTC-bucketed /2026-02/", got)
	}
}

func TestStoragePath_AllFormatsHaveExtensions(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	when := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	cases := map[Format]string{
		FormatPNG:  ".png",
		FormatJPEG: ".jpg",
		FormatWEBP: ".webp",
		FormatPDF:  ".pdf",
	}
	for f, ext := range cases {
		t.Run(string(f), func(t *testing.T) {
			got, err := StoragePath(tenant, when, Result{Hash: "h", Format: f})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !strings.HasSuffix(got, ext) {
				t.Fatalf("path %q lacks expected suffix %q", got, ext)
			}
		})
	}
}

func TestStoragePath_LowercasesHash(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	when := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	r := Result{Hash: "ABCDEF", Format: FormatPNG}
	got, err := StoragePath(tenant, when, r)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(got, "abcdef") {
		t.Fatalf("path %q did not lowercase the hash", got)
	}
	if strings.Contains(got, "ABCDEF") {
		t.Fatalf("path %q still contains uppercase hash", got)
	}
}

func TestStoragePath_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	_, err := StoragePath(uuid.Nil, time.Now(), Result{Hash: "x", Format: FormatPNG})
	if err == nil {
		t.Fatal("err = nil, want non-nil for uuid.Nil")
	}
}

func TestStoragePath_RejectsEmptyHash(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	_, err := StoragePath(tenant, time.Now(), Result{Hash: "", Format: FormatPNG})
	if !errors.Is(err, ErrEmptyHash) {
		t.Fatalf("err = %v, want ErrEmptyHash", err)
	}
}

func TestStoragePath_RejectsUnknownFormat(t *testing.T) {
	t.Parallel()
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	_, err := StoragePath(tenant, time.Now(), Result{Hash: "x", Format: Format("svg")})
	if err == nil {
		t.Fatal("err = nil, want non-nil for unknown format")
	}
}
