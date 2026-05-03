package serve_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/serve"
)

func TestNewURLBuilder_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []string{"", "static.crm.example.com", "ftp://x"}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if _, err := serve.NewURLBuilder(c); err == nil {
				t.Fatalf("NewURLBuilder(%q) returned nil error", c)
			}
		})
	}
}

func TestNewURLBuilder_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	b, err := serve.NewURLBuilder("https://static.crm.example.com/")
	if err != nil {
		t.Fatalf("NewURLBuilder: %v", err)
	}
	tid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	got := b.LogoURL(tid)
	if strings.HasPrefix(got, "https://static.crm.example.com//") {
		t.Fatalf("trailing slash leaked: %q", got)
	}
	want := "https://static.crm.example.com/t/" + tid.String() + "/logo"
	if got != want {
		t.Fatalf("LogoURL = %q, want %q", got, want)
	}
}

func TestURLBuilder_MediaURL(t *testing.T) {
	t.Parallel()
	b, err := serve.NewURLBuilder("https://static.crm.example.com")
	if err != nil {
		t.Fatalf("NewURLBuilder: %v", err)
	}
	tid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	hash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	got := b.MediaURL(tid, hash)
	want := "https://static.crm.example.com/t/" + tid.String() + "/m/" + hash
	if got != want {
		t.Fatalf("MediaURL = %q, want %q", got, want)
	}
}

func TestURLBuilder_HTTPOriginAccepted(t *testing.T) {
	t.Parallel()
	// Local dev / smoke tests use http:// — the helper must support it.
	b, err := serve.NewURLBuilder("http://localhost:8081")
	if err != nil {
		t.Fatalf("NewURLBuilder: %v", err)
	}
	tid := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	got := b.LogoURL(tid)
	if !strings.HasPrefix(got, "http://localhost:8081/t/") {
		t.Fatalf("LogoURL = %q", got)
	}
}
