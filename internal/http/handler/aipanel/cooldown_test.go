package aipanel

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCooldownFragment_QuotaReason_ContainsExpectedAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := CooldownFragment(&buf, 30*time.Second, "quota"); err != nil {
		t.Fatalf("CooldownFragment err = %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`disabled`,
		`hx-disable-elt="this"`,
		`--cooldown-duration:30000ms`,
		`Próxima geração em 30 s`,
		`data-reason="quota"`,
		`aria-disabled="true"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

func TestCooldownFragment_BackendUnavailable_DifferentCopy(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := CooldownFragment(&buf, 5*time.Second, "backend_unavailable"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), "AI panel indisponível") {
		t.Fatalf("output missing backend_unavailable copy:\n%s", buf.String())
	}
}

func TestCooldownFragment_RoundsSecondsUp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := CooldownFragment(&buf, 1500*time.Millisecond, "quota"); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Próxima geração em 2 s") {
		t.Fatalf("expected '2 s' caption (1.5s rounds up); got:\n%s", out)
	}
	if !strings.Contains(out, "--cooldown-duration:1500ms") {
		t.Fatalf("expected --cooldown-duration:1500ms; got:\n%s", out)
	}
}

func TestCooldownFragment_NegativeRetryClamps(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := CooldownFragment(&buf, -2*time.Second, "quota"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), "em 1 s") {
		t.Fatalf("expected clamp to 1s; got:\n%s", buf.String())
	}
}

func TestCooldownFragment_OnHTTPResponseWriter_SetsContentType(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := CooldownFragment(rec, time.Second, "quota"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestCooldownFragment_DoesNotOverrideExistingContentType(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/html; charset=utf-8; profile=existing")
	if err := CooldownFragment(rec, time.Second, "quota"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8; profile=existing" {
		t.Fatalf("Content-Type changed: %q", got)
	}
}

func TestCooldownRenderer_WritesFragment(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	CooldownRenderer(rec, req, 6*time.Second, "quota")
	if !strings.Contains(rec.Body.String(), "--cooldown-duration:6000ms") {
		t.Fatalf("body missing cooldown attr:\n%s", rec.Body.String())
	}
}

func TestFragmentFromHeaders_PrefersMs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := FragmentFromHeaders(&buf, "30", "12500", "quota"); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "--cooldown-duration:12500ms") {
		t.Fatalf("expected ms-precision; got:\n%s", out)
	}
	// 12500ms = 13s rounded up.
	if !strings.Contains(out, "Próxima geração em 13 s") {
		t.Fatalf("expected 13 s caption; got:\n%s", out)
	}
}

func TestFragmentFromHeaders_FallsBackToSeconds(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := FragmentFromHeaders(&buf, "30", "", "quota"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), "--cooldown-duration:30000ms") {
		t.Fatalf("fallback failed:\n%s", buf.String())
	}
}

func TestFragmentFromHeaders_BothMissingDefaultsToOneSecond(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := FragmentFromHeaders(&buf, "", "", "quota"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), "--cooldown-duration:1000ms") {
		t.Fatalf("default failed:\n%s", buf.String())
	}
}

func TestCooldownFragment_EscapesUnknownReason(t *testing.T) {
	t.Parallel()
	// Reason flows into a data attribute via html/template, which must
	// HTML-escape it. This is a defense-in-depth check — quote in reason
	// would break the attribute if it leaked.
	var buf bytes.Buffer
	if err := CooldownFragment(&buf, time.Second, `quota" onfocus="alert(1)`); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `onfocus="alert(1)`) {
		t.Fatalf("unsanitized output:\n%s", out)
	}
}
