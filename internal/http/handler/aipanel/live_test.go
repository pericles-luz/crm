package aipanel

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLiveButton_HasSwapTargetAndHTMXAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := LiveButton(&buf, LiveButtonOptions{PostPath: "/ai/panel/regen"}); err != nil {
		t.Fatalf("LiveButton err = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`id="ai-panel-regenerate"`,
		`class="ai-panel-regenerate"`,
		`hx-post="/ai/panel/regen"`,
		`hx-target="#ai-panel-regenerate"`,
		`hx-swap="outerHTML"`,
		`hx-disable-elt="this"`,
		`type="button"`,
		`Regenerar`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

func TestLiveButton_DefaultLabelIsRegenerar(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := LiveButton(&buf, LiveButtonOptions{PostPath: "/ai/panel/regen"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), ">Regenerar<") {
		t.Fatalf("expected default label 'Regenerar'; got:\n%s", buf.String())
	}
}

func TestLiveButton_CustomLabelOverridesDefault(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := LiveButton(&buf, LiveButtonOptions{
		PostPath: "/ai/panel/regen",
		Label:    "Gerar novamente",
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, ">Gerar novamente<") {
		t.Fatalf("custom label missing; got:\n%s", out)
	}
	if strings.Contains(out, ">Regenerar<") {
		t.Fatalf("default label should not appear when custom label set; got:\n%s", out)
	}
}

func TestLiveButton_EmptyPostPathReturnsSentinelError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := LiveButton(&buf, LiveButtonOptions{})
	if err == nil {
		t.Fatal("expected error for empty PostPath")
	}
	if !errors.Is(err, ErrLiveButtonPostPathRequired) {
		t.Fatalf("err = %v, want ErrLiveButtonPostPathRequired", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output on error, got %q", buf.String())
	}
}

func TestLiveButton_OnHTTPResponseWriter_SetsContentType(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := LiveButton(rec, LiveButtonOptions{PostPath: "/x"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
}

func TestLiveButton_DoesNotOverrideExistingContentType(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/html; charset=utf-8; profile=existing")
	if err := LiveButton(rec, LiveButtonOptions{PostPath: "/x"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8; profile=existing" {
		t.Fatalf("Content-Type changed: %q", got)
	}
}

func TestLiveButton_EscapesPostPath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// Defense-in-depth: if a caller forwards a tainted path the URL
	// context of html/template should HTML-escape attribute terminators.
	if err := LiveButton(&buf, LiveButtonOptions{
		PostPath: `/ai/regen" onfocus="alert(1)`,
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(buf.String(), `onfocus="alert(1)`) {
		t.Fatalf("unsanitized output:\n%s", buf.String())
	}
}

func TestLiveButton_EscapesLabel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := LiveButton(&buf, LiveButtonOptions{
		PostPath: "/x",
		Label:    `<script>alert(1)</script>`,
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(buf.String(), `<script>alert(1)</script>`) {
		t.Fatalf("unsanitized label leaked into output:\n%s", buf.String())
	}
}

// Stable-swap-target invariant: the live button id MUST equal the cooldown
// fragment id, otherwise hx-swap="outerHTML" cannot target it. This test is
// the contract that keeps the two renderers in sync.
func TestLiveButton_SwapTargetMatchesCooldownFragment(t *testing.T) {
	t.Parallel()
	const id = `id="ai-panel-regenerate"`

	var live bytes.Buffer
	if err := LiveButton(&live, LiveButtonOptions{PostPath: "/x"}); err != nil {
		t.Fatalf("LiveButton err = %v", err)
	}
	if !strings.Contains(live.String(), id) {
		t.Fatalf("live button missing %s:\n%s", id, live.String())
	}

	var cool bytes.Buffer
	if err := CooldownFragment(&cool, 0, "quota"); err != nil {
		t.Fatalf("CooldownFragment err = %v", err)
	}
	if !strings.Contains(cool.String(), id) {
		t.Fatalf("cooldown fragment missing %s:\n%s", id, cool.String())
	}
}

func TestStylesheetLink_DefaultHref(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := StylesheetLink(&buf, ""); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	want := `<link rel="stylesheet" href="/static/css/aipanel.css">`
	if !strings.Contains(out, want) {
		t.Fatalf("output missing default link tag %q\nfull:\n%s", want, out)
	}
}

func TestStylesheetLink_CustomHref(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := StylesheetLink(&buf, "/assets/aipanel.v2.css"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), `href="/assets/aipanel.v2.css"`) {
		t.Fatalf("custom href missing:\n%s", buf.String())
	}
}

func TestStylesheetLink_OnHTTPResponseWriter_SetsContentType(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := StylesheetLink(rec, ""); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestStylesheetLink_EscapesHref(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := StylesheetLink(&buf, `/x" onerror="alert(1)`); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(buf.String(), `onerror="alert(1)`) {
		t.Fatalf("unsanitized output:\n%s", buf.String())
	}
}

// Default href stays in sync with the constant — this catches accidental
// drift if someone moves the file again.
func TestDefaultStylesheetHref_MatchesServedPath(t *testing.T) {
	t.Parallel()
	if DefaultStylesheetHref != "/static/css/aipanel.css" {
		t.Fatalf("DefaultStylesheetHref = %q, want /static/css/aipanel.css "+
			"(must match the path served by the FileServer rooted at web/static/)",
			DefaultStylesheetHref)
	}
}
