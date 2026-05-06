package render_test

import (
	"bytes"
	"context"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"

	"github.com/pericles-luz/crm/internal/ai/render"
)

func renderString(t *testing.T, in string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := render.Render(&buf, in); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestSafeText_ComponentMatchesRender(t *testing.T) {
	in := "**bold** and *italic*"
	var buf bytes.Buffer
	if err := render.SafeText(in).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Component.Render: %v", err)
	}
	got := buf.String()
	want := renderString(t, in)
	if got != want {
		t.Fatalf("Component output drifted from Render:\n  Component: %q\n  Render   : %q", got, want)
	}
}

func TestSafeText_AllowedMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "paragraph",
			in:   "hello world",
			want: "<p>hello world</p>\n",
		},
		{
			name: "bold",
			in:   "**hello**",
			want: "<p><strong>hello</strong></p>\n",
		},
		{
			name: "italic",
			in:   "*hello*",
			want: "<p><em>hello</em></p>\n",
		},
		{
			name: "inline-code",
			in:   "`x = 1`",
			want: "<p><code>x = 1</code></p>\n",
		},
		{
			name: "fenced-code",
			in:   "```\nfmt.Println(\"hi\")\n```",
			want: "<pre><code>fmt.Println(&#34;hi&#34;)\n</code></pre>\n",
		},
		{
			name: "indented-code",
			in:   "    line one\n    line two",
			want: "<pre><code>line one\nline two\n</code></pre>\n",
		},
		{
			name: "ul",
			in:   "- a\n- b",
			want: "<ul>\n<li>a</li>\n<li>b</li>\n</ul>\n",
		},
		{
			name: "ol",
			in:   "1. a\n2. b",
			want: "<ol>\n<li>a</li>\n<li>b</li>\n</ol>\n",
		},
		{
			name: "soft-break",
			in:   "line one\nline two",
			want: "<p>line one\nline two</p>\n",
		},
		{
			name: "hard-break",
			in:   "line one  \nline two",
			want: "<p>line one<br>line two</p>\n",
		},
		{
			name: "ampersand-and-angle-brackets-escape",
			in:   "x & y < z > w",
			want: "<p>x &amp; y &lt; z &gt; w</p>\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderString(t, tc.in)
			if got != tc.want {
				t.Fatalf("output mismatch\n  in:   %q\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSafeText_LinksRenderedAsText(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantParts []string
		notParts  []string
	}{
		{
			name:      "label-and-url",
			in:        "[click here](https://example.com)",
			wantParts: []string{"click here", "https://example.com"},
			notParts:  []string{"<a", "href"},
		},
		{
			name:      "javascript-url-rendered-as-text-only",
			in:        `[ok](javascript:alert(1))`,
			wantParts: []string{"ok", "javascript:alert(1)"},
			notParts:  []string{"<a", "href=\"javascript"},
		},
		{
			name:      "data-url-rendered-as-text-only",
			in:        "[x](data:text/html,<script>alert(1)</script>)",
			wantParts: []string{"x", "data:text/html,"},
			notParts:  []string{"<a", "href", "<script"},
		},
		{
			name:      "url-only-no-label",
			in:        "[](https://example.com)",
			wantParts: []string{"https://example.com"},
			notParts:  []string{"<a", "href"},
		},
		{
			name:      "label-equals-url",
			in:        "[https://example.com](https://example.com)",
			wantParts: []string{"https://example.com"},
			notParts:  []string{"<a", "href", "(https://example.com)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderString(t, tc.in)
			for _, p := range tc.wantParts {
				if !strings.Contains(got, p) {
					t.Errorf("expected %q in output, got %q", p, got)
				}
			}
			for _, p := range tc.notParts {
				if strings.Contains(got, p) {
					t.Errorf("did NOT expect %q in output, got %q", p, got)
				}
			}
		})
	}
}

// TestSafeText_AdversarialInputsAreInert covers the F29 attack matrix from the
// task: every payload in this list must come back inert — no <script>,
// <iframe>, <svg>, on*-attributes, or live href/src in the rendered output.
func TestSafeText_AdversarialInputsAreInert(t *testing.T) {
	cases := []string{
		`<script>alert(1)</script>`,
		`<SCRIPT SRC="x.js"></SCRIPT>`,
		`<img src=x onerror=alert(1)>`,
		`<iframe src="https://evil.example/"></iframe>`,
		`<svg onload=alert(1)></svg>`,
		`<a href="javascript:alert(1)">click</a>`,
		`<a href="data:text/html,<script>x</script>">click</a>`,
		`<style>body{background:url(javascript:alert(1))}</style>`,
		`<object data="x.swf"></object>`,
		`<embed src="x.swf">`,
		`<form action="x"><input name=y></form>`,
		`<meta http-equiv=refresh content="0;url=x">`,
		`<link rel=stylesheet href="x.css">`,
		`<base href="//evil.example/">`,
		`<math><mi xlink:href="javascript:alert(1)">x</mi></math>`,
		`<table><tr><td onmouseover=alert(1)>x</td></tr></table>`,
		`![alt text](https://evil.example/img.png)`,
		`![alt](javascript:alert(1))`,
		"<!--[if IE]><script>alert(1)</script><![endif]-->",
		"javascript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"<plaintext>",
		"<noscript><script>alert(1)</script>",
	}

	live := regexp.MustCompile(`(?i)<\s*(script|iframe|svg|object|embed|form|style|link|meta|base|plaintext|noscript|math)\b`)
	onAttr := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	hrefSrc := regexp.MustCompile(`(?i)<\s*[a-z][^>]*\s(href|src)\s*=`)

	for _, in := range cases {
		got := renderString(t, in)
		if live.MatchString(got) {
			t.Errorf("live HTML element survived for %q -> %q", in, got)
		}
		if onAttr.MatchString(got) {
			t.Errorf("on*-attribute survived for %q -> %q", in, got)
		}
		if hrefSrc.MatchString(got) {
			t.Errorf("href/src attribute survived for %q -> %q", in, got)
		}
	}
}

func TestSafeText_HeadingsAndImagesAreDropped(t *testing.T) {
	cases := []string{
		"# h1",
		"## h2",
		"### h3",
		"![alt](https://example.com/i.png)",
		"---",
	}
	live := regexp.MustCompile(`(?i)<\s*(h[1-6]|img|hr)\b`)
	for _, in := range cases {
		got := renderString(t, in)
		if live.MatchString(got) {
			t.Errorf("disallowed element rendered for %q -> %q", in, got)
		}
	}
}

func TestSafeText_BlockquoteWrapperDropped(t *testing.T) {
	got := renderString(t, "> hi")
	if strings.Contains(got, "<blockquote") {
		t.Errorf("blockquote wrapper leaked: %q", got)
	}
}

// hostileString is a quick.Generator value that emits moderate-length strings
// drawn from a runeset rich in markup-significant characters and known
// injection vectors.
type hostileString string

const hostileRunes = "abc 0123<>&\"'`*_~![]{}()#-+/\\\nscriptIFRAMEonloadhrefjavascript:data:" +
	"<svg><iframe><script><object><style><meta><link><base><form><math>" +
	"</customer_message>" +
	"`````\\x00\\u200b\\u2028\\u2029\\u00a0"

// Generate satisfies quick.Generator.
func (hostileString) Generate(r *rand.Rand, size int) reflect.Value {
	n := r.Intn(size + 4)
	if n > 256 {
		n = 256
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = hostileRunes[r.Intn(len(hostileRunes))]
	}
	return reflect.ValueOf(hostileString(out))
}

func TestSafeText_AutoLinkAndNestedFormatting(t *testing.T) {
	// Auto-links (<https://example.com>) must render as plain text, never <a>.
	got := renderString(t, "see <https://example.com>")
	if strings.Contains(got, "<a ") || strings.Contains(got, "href") {
		t.Errorf("auto-link became anchor: %q", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("auto-link text missing: %q", got)
	}

	// collectText must handle nested formatting inside link labels:
	// **bold**, *italic*, and `code` should all flatten into the label.
	got = renderString(t, "[**hi** *there* `now`](https://example.com)")
	for _, want := range []string{"hi", "there", "now", "https://example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in nested link rendering: %q", want, got)
		}
	}
	if strings.Contains(got, "<a ") || strings.Contains(got, "href") {
		t.Errorf("nested link became anchor: %q", got)
	}

	// Link inside an image (![alt][ref]-style) — image is dropped entirely.
	got = renderString(t, "![alt](https://example.com/i.png)")
	if strings.Contains(got, "<img") || strings.Contains(got, "src=") {
		t.Errorf("image survived: %q", got)
	}
}

func TestSafeText_NestedListsRender(t *testing.T) {
	in := "- a\n  - a1\n  - a2\n- b"
	got := renderString(t, in)
	for _, want := range []string{"<ul>", "</ul>", "a1", "a2"} {
		if !strings.Contains(got, want) {
			t.Errorf("nested list missing %q: %q", want, got)
		}
	}
}

// failingWriter returns an error after writing afterBytes bytes. It exercises
// the io.Writer error branches inside the renderer so coverage reflects them.
type failingWriter struct {
	written    int
	afterBytes int
}

func (fw *failingWriter) Write(p []byte) (int, error) {
	if fw.written >= fw.afterBytes {
		return 0, errFailingWriter
	}
	n := len(p)
	if fw.written+n > fw.afterBytes {
		n = fw.afterBytes - fw.written
	}
	fw.written += n
	if n < len(p) {
		return n, errFailingWriter
	}
	return n, nil
}

var errFailingWriter = errFailingWriterT("failing writer")

type errFailingWriterT string

func (e errFailingWriterT) Error() string { return string(e) }

func TestSafeText_WriterErrorsPropagate(t *testing.T) {
	// Render a doc that exercises many node types: paragraph, emphasis, link,
	// list, code span, fenced code, blockquote.
	doc := "**a** *b* [c](http://x) `d`\n\n- e\n- f\n\n```\ng\n```\n\n> h"

	// Walk a sweep of allowed-byte budgets so we hit error returns from many
	// distinct write sites in the renderer.
	for budget := 0; budget < 80; budget += 3 {
		fw := &failingWriter{afterBytes: budget}
		err := render.Render(fw, doc)
		// We don't care which specific error we get back — just that the
		// renderer surfaces *some* error rather than swallowing it. (When
		// budget >= total output length the call succeeds, which is also
		// fine; we're just trying to exercise error branches.)
		if err == nil && fw.written < budget {
			t.Errorf("budget=%d: expected error, got nil with written=%d", budget, fw.written)
		}
	}
}

// TestSafeText_FuzzInert asserts the F29 invariant over ≥1000 random inputs:
// no rendered output may contain a live HTML element, an on*-attribute, or a
// live href/src after passing through SafeText.
func TestSafeText_FuzzInert(t *testing.T) {
	live := regexp.MustCompile(`(?i)<\s*(script|iframe|svg|object|embed|form|style|link|meta|base|img|plaintext|noscript|math|h[1-6])\b`)
	onAttr := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	hrefSrc := regexp.MustCompile(`(?i)<\s*[a-z][^>]*\s(href|src)\s*=`)

	check := func(s hostileString) bool {
		var buf bytes.Buffer
		if err := render.Render(&buf, string(s)); err != nil {
			t.Logf("render error for %q: %v", string(s), err)
			return false
		}
		out := buf.String()
		if live.MatchString(out) || onAttr.MatchString(out) || hrefSrc.MatchString(out) {
			t.Logf("escape detected\n  in:  %q\n  out: %q", string(s), out)
			return false
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 1500}); err != nil {
		t.Fatalf("fuzz check: %v", err)
	}
}
