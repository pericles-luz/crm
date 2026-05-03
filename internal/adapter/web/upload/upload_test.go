package upload

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRender_LogoForm_AppliesDefaults(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Render(&buf, KindLogo, FormConfig{}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	wantContains := []string{
		`data-upload="logo"`,
		`data-upload-allowed="png,jpeg,webp"`,
		`data-upload-max-bytes="2097152"`,
		`accept="image/png,image/jpeg,image/webp"`,
		`hx-post="/uploads/logo"`,
		`hx-target="this"`,
		`hx-encoding="multipart/form-data"`,
		`enctype="multipart/form-data"`,
		`id="sin-upload-logo-input"`,
		`for="sin-upload-logo-input"`,
		`aria-describedby="sin-upload-logo-error"`,
		`role="status"`,
		`aria-live="polite"`,
		`data-upload-error`,
		`data-upload-cancel`,
		`data-upload-preview`,
		`Logo da empresa (PNG, JPG ou WEBP)`,
		`Tamanho máximo: 2MB.`,
	}
	for _, want := range wantContains {
		if !strings.Contains(out, want) {
			t.Errorf("logo form missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `image/svg+xml`) {
		t.Errorf("logo form must NOT advertise image/svg+xml accept; rendered:\n%s", out)
	}
}

func TestRender_LogoForm_OverridesApplied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := FormConfig{
		ID:            "co-logo",
		Label:         "Envie o logo",
		Action:        "/tenant/abc/logo",
		Target:        "#logo-status",
		MaxBytes:      512 * 1024,
		CSRFFieldName: "csrf",
		CSRFToken:     "tok-123",
	}
	if err := Render(&buf, KindLogo, cfg); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	wantContains := []string{
		`id="co-logo-input"`,
		`Envie o logo`,
		`hx-post="/tenant/abc/logo"`,
		`action="/tenant/abc/logo"`,
		`hx-target="#logo-status"`,
		`data-upload-max-bytes="524288"`,
		`Tamanho máximo: 512KB.`,
		`name="csrf"`,
		`value="tok-123"`,
	}
	for _, want := range wantContains {
		if !strings.Contains(out, want) {
			t.Errorf("logo form missing %q in:\n%s", want, out)
		}
	}
}

func TestRender_LogoForm_OmitsCSRFInputWhenEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Render(&buf, KindLogo, FormConfig{}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(buf.String(), `name=""`) {
		t.Fatalf("rendered an empty-name CSRF input — should be omitted entirely")
	}
	if strings.Contains(buf.String(), `type="hidden"`) {
		t.Fatalf("hidden input rendered without CSRF data; output:\n%s", buf.String())
	}
}

func TestRender_AttachmentForm_AppliesDefaults(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Render(&buf, KindAttachment, FormConfig{}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	wantContains := []string{
		`data-upload="attachment"`,
		`data-upload-allowed="png,jpeg,webp,pdf"`,
		`data-upload-max-bytes="20971520"`,
		`accept="image/png,image/jpeg,image/webp,application/pdf"`,
		`hx-post="/uploads/attachment"`,
		`Anexo (PNG, JPG, WEBP ou PDF)`,
		`Tamanho máximo: 20MB.`,
	}
	for _, want := range wantContains {
		if !strings.Contains(out, want) {
			t.Errorf("attachment form missing %q", want)
		}
	}
}

func TestRender_UnknownKind_ReturnsErrUnknownKind(t *testing.T) {
	t.Parallel()
	err := Render(&bytes.Buffer{}, Kind("nope"), FormConfig{})
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v, want ErrUnknownKind", err)
	}
}

func TestRender_HTMLEscaping_NeutralisesXSS(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := FormConfig{
		Label:         `<script>alert("xss")</script>`,
		Action:        `/x"><img/src=x onerror=alert(1)>`,
		CSRFFieldName: "csrf",
		CSRFToken:     `"><script>1</script>`,
	}
	if err := Render(&buf, KindLogo, cfg); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	// Tag-open `<script>` from user input must never reach the rendered
	// HTML un-escaped; html/template should turn `<` into `&lt;`.
	if strings.Contains(out, "<script>alert(") {
		t.Fatalf("user-supplied <script> tag survived escaping:\n%s", out)
	}
	if strings.Contains(out, `<script>1</script>`) {
		t.Fatalf("user-supplied CSRF script payload survived escaping:\n%s", out)
	}
	// User-supplied `"` characters must be entity-escaped inside attributes
	// (so they cannot break out of the attribute value).
	if strings.Contains(out, `value="">`) {
		t.Fatalf("CSRF token closed its attribute prematurely (escaping failed):\n%s", out)
	}
	// Belt-and-suspenders: confirm the escapes we expect *did* land.
	if !strings.Contains(out, `&lt;script&gt;`) {
		t.Fatalf("expected entity-encoded <script> in output; got:\n%s", out)
	}
	if !strings.Contains(out, `&#34;`) {
		t.Fatalf("expected entity-encoded quotes in output; got:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0KB"},
		{-1, "0KB"},
		{1, "1KB"},
		{512, "1KB"},
		{1024, "1KB"},
		{1024 * 100, "100KB"},
		{2 * 1024 * 1024, "2MB"},
		{20 * 1024 * 1024, "20MB"},
		{1024 * 1024, "1MB"},
		{1500 * 1024, "1.5MB"},
	}
	for _, c := range cases {
		got := HumanBytes(c.in)
		if got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStaticFS_ServesEmbeddedAssets(t *testing.T) {
	t.Parallel()
	files := []string{"upload.js", "upload.css", "templates.html"}
	sfs := StaticFS()
	for _, name := range files {
		f, err := sfs.Open(name)
		if err != nil {
			t.Errorf("StaticFS open %s: %v", name, err)
			continue
		}
		st, err := f.Stat()
		_ = f.Close()
		if err != nil {
			t.Errorf("Stat %s: %v", name, err)
			continue
		}
		if st.Size() == 0 {
			t.Errorf("StaticFS %s is empty", name)
		}
	}
}

func TestStaticFS_RejectsTraversal(t *testing.T) {
	t.Parallel()
	sfs := StaticFS()
	if _, err := sfs.Open("../upload.go"); err == nil {
		t.Fatalf("traversal returned nil error — fs.Sub should constrain paths")
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
		// fs.Sub returns ErrInvalid for ".." paths; ErrNotExist is also ok.
		t.Logf("traversal error (expected): %v", err)
	}
}

func TestStaticHandler_ServesUploadJS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.StripPrefix("/static/", StaticHandler()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/upload.js")
	if err != nil {
		t.Fatalf("GET upload.js: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") &&
		!strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("Content-Type = %q, want text/javascript or application/javascript", ct)
	}
	body := make([]byte, 64)
	if _, err := resp.Body.Read(body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "upload.js") && !strings.Contains(string(body), "SIN-62258") {
		t.Fatalf("served file does not look like our upload.js: %q", string(body))
	}
}

func TestResolve_DefaultsForLogoAndAttachment(t *testing.T) {
	t.Parallel()
	dLogo, nameLogo, err := resolve(KindLogo, FormConfig{})
	if err != nil {
		t.Fatalf("resolve logo: %v", err)
	}
	if nameLogo != "logoForm" {
		t.Errorf("logo name = %q, want logoForm", nameLogo)
	}
	if dLogo.MaxBytes != LogoMaxBytes {
		t.Errorf("logo MaxBytes = %d, want %d", dLogo.MaxBytes, LogoMaxBytes)
	}
	if dLogo.Action != "/uploads/logo" {
		t.Errorf("logo Action = %q, want /uploads/logo", dLogo.Action)
	}
	if dLogo.MaxBytesHuman != "2MB" {
		t.Errorf("logo human = %q, want 2MB", dLogo.MaxBytesHuman)
	}

	dAtt, nameAtt, err := resolve(KindAttachment, FormConfig{})
	if err != nil {
		t.Fatalf("resolve attachment: %v", err)
	}
	if nameAtt != "attachmentForm" {
		t.Errorf("attachment name = %q, want attachmentForm", nameAtt)
	}
	if dAtt.MaxBytes != AttachmentMaxBytes {
		t.Errorf("attachment MaxBytes = %d, want %d", dAtt.MaxBytes, AttachmentMaxBytes)
	}
	if dAtt.MaxBytesHuman != "20MB" {
		t.Errorf("attachment human = %q, want 20MB", dAtt.MaxBytesHuman)
	}
}

func TestFirstNonEmptyAndPickInt64(t *testing.T) {
	t.Parallel()
	if got := firstNonEmpty("", "fallback"); got != "fallback" {
		t.Errorf("firstNonEmpty empty got %q", got)
	}
	if got := firstNonEmpty("   ", "fallback"); got != "fallback" {
		t.Errorf("firstNonEmpty whitespace got %q", got)
	}
	if got := firstNonEmpty("ok", "fallback"); got != "ok" {
		t.Errorf("firstNonEmpty got %q", got)
	}
	if got := pickInt64(0, 99); got != 99 {
		t.Errorf("pickInt64 zero got %d", got)
	}
	if got := pickInt64(7, 99); got != 7 {
		t.Errorf("pickInt64 nonzero got %d", got)
	}
	if got := pickInt64(-3, 99); got != 99 {
		t.Errorf("pickInt64 negative got %d", got)
	}
}

func TestRound1(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{1.04, 1.0},
		{1.05, 1.1},
		{1.5, 1.5},
		{-1.05, -1.1},
		{-0.05, -0.1},
	}
	for _, c := range cases {
		got := round1(c.in)
		if got != c.want {
			t.Errorf("round1(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
