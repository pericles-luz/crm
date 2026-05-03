//go:build e2e

package upload_test

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	uploadweb "github.com/pericles-luz/crm/internal/adapter/web/upload"
)

// CSS / JS selectors, kept here so a template tweak stays a single-edit fix.
const (
	formSel       = "form[data-upload='logo']"
	fileInputSel  = "input#sin-upload-logo-input"
	submitBtnSel  = formSel + " button[type='submit']"
	cancelBtnSel  = formSel + " [data-upload-cancel]"
	errorSlotSel  = "#sin-upload-logo-error"
	successSel    = "[data-upload-success]"
	progressWrap  = formSel + " [data-upload-progress]"
	previewSel    = formSel + " canvas[data-upload-preview]"
	uploadAction  = "/uploads/logo"
	expectedSVG   = uploadweb.MsgUnsupportedLogo
	expectedXSV   = uploadweb.MsgUnsupportedLogo
	expected415   = uploadweb.MsgServerRejected
	expectedCanc  = uploadweb.MsgCancelled
	successText   = "Logo enviado com sucesso."
)

// trimEqual normalises whitespace so PT-BR strings render identically
// across textContent / innerHTML round trips.
func trimEqual(t *testing.T, got, want, label string) {
	t.Helper()
	g := strings.TrimSpace(got)
	if g != want {
		t.Fatalf("%s: got %q want %q", label, g, want)
	}
}

// Scenario 1: SVG file selected → magic-byte gate rejects client-side,
// PT-BR error visible, no XHR sent.
func TestE2E_SVGBlockedClientSide(t *testing.T) {
	srv, counter := startServer(t, postBoom)
	ctx := withBrowser(t)

	var errText string
	var calls []xhrCall
	runOrFail(t, ctx, "scenario-1",
		chromedp.Navigate(formPageURL(srv)),
		chromedp.WaitVisible(formSel, chromedp.ByQuery),
		chromedp.Evaluate(xhrSpyJS, nil),
		chromedp.SetUploadFiles(fileInputSel, []string{fixturePath(t, "logo.svg")}, chromedp.ByQuery),
		chromedp.WaitVisible(errorSlotSel, chromedp.ByQuery),
		chromedp.Text(errorSlotSel, &errText, chromedp.ByQuery),
		chromedp.Evaluate(`window.__sinXHRSpy || []`, &calls),
	)

	trimEqual(t, errText, expectedSVG, "error message")

	for _, c := range calls {
		if c.Method == "POST" && strings.Contains(c.URL, uploadAction) {
			t.Fatalf("expected no POST to %s, got %+v", uploadAction, calls)
		}
	}
	if got := counter.get(); got != 0 {
		t.Fatalf("server saw %d POST(s); expected 0", got)
	}
}

// Scenario 2: PNG bytes saved with .svg filename → magic bytes win,
// the form proceeds and the server records the upload.
func TestE2E_PNGRenamedSVGAccepted(t *testing.T) {
	srv, counter := startServer(t, postOK)
	ctx := withBrowser(t)

	var calls []xhrCall
	steps := []namedAction{
		{"navigate", chromedp.Navigate(formPageURL(srv))},
		{"wait-form", chromedp.WaitVisible(formSel, chromedp.ByQuery)},
		{"wait-htmx", waitHTMXReady()},
		{"install-spy", chromedp.Evaluate(xhrSpyJS, nil)},
		{"set-file", chromedp.SetUploadFiles(fileInputSel, []string{fixturePath(t, "png-as-svg.svg")}, chromedp.ByQuery)},
		// classify() resolves asynchronously (FileReader). On reject it
		// clears input.value. We poll until the input has stabilised:
		// either still set (classify accepted) or cleared (rejected).
		// The preview canvas is NOT a reliable signal because img.onload
		// fails when the file's MIME (image/svg+xml from .svg extension)
		// disagrees with the actual PNG bytes.
		{"wait-classify-settled", waitClassifySettled(fileInputSel)},
		{"assert-file-kept", assertJSTrue(`(function(){
			var i = document.querySelector("input#sin-upload-logo-input");
			return i && i.files && i.files.length === 1;
		})()`, "magic-byte gate cleared the file (expected accept)")},
		{"click-submit", chromedp.Click(submitBtnSel, chromedp.ByQuery)},
		{"wait-server-post", waitServerPOST(counter, 1)},
		{"wait-success-swap", chromedp.WaitVisible(successSel, chromedp.ByQuery)},
		{"read-spy", chromedp.Evaluate(`window.__sinXHRSpy || []`, &calls)},
	}
	runSteps(t, ctx, "scenario-2", steps)

	if got := counter.get(); got != 1 {
		t.Fatalf("server saw %d POST(s); expected 1", got)
	}
	if !hasUploadPost(calls) {
		t.Fatalf("expected POST to %s in spy buffer, got %+v", uploadAction, calls)
	}
}

// Scenario 3: EXE bytes saved with .png filename → magic-byte gate
// rejects, PT-BR error visible, no XHR sent.
func TestE2E_EXERenamedPNGRejected(t *testing.T) {
	srv, counter := startServer(t, postBoom)
	ctx := withBrowser(t)

	var errText string
	var calls []xhrCall
	runOrFail(t, ctx, "scenario-3",
		chromedp.Navigate(formPageURL(srv)),
		chromedp.WaitVisible(formSel, chromedp.ByQuery),
		chromedp.Evaluate(xhrSpyJS, nil),
		chromedp.SetUploadFiles(fileInputSel, []string{fixturePath(t, "exe-as-png.png")}, chromedp.ByQuery),
		chromedp.WaitVisible(errorSlotSel, chromedp.ByQuery),
		chromedp.Text(errorSlotSel, &errText, chromedp.ByQuery),
		chromedp.Evaluate(`window.__sinXHRSpy || []`, &calls),
	)

	trimEqual(t, errText, expectedXSV, "error message")
	if hasUploadPost(calls) {
		t.Fatalf("expected no POST to %s, got %+v", uploadAction, calls)
	}
	if got := counter.get(); got != 0 {
		t.Fatalf("server saw %d POST(s); expected 0", got)
	}
}

// Scenario 4: client-valid PNG → server returns 415 → upload.js
// translates the status to PT-BR via messageForStatus and renders it
// in the form's error slot.
func TestE2E_Server415Rendered(t *testing.T) {
	srv, counter := startServer(t, post415)
	ctx := withBrowser(t)

	var errText string
	runOrFail(t, ctx, "scenario-4",
		chromedp.Navigate(formPageURL(srv)),
		chromedp.WaitVisible(formSel, chromedp.ByQuery),
		chromedp.SetUploadFiles(fileInputSel, []string{fixturePath(t, "logo.png")}, chromedp.ByQuery),
		chromedp.WaitVisible(previewSel, chromedp.ByQuery),
		chromedp.Click(submitBtnSel, chromedp.ByQuery),
		// Wait until the error slot becomes visible AND its textContent
		// matches the canonical PT-BR string. textContent matching alone
		// would be racy because the slot is created visible-then-text.
		waitErrorEquals(errorSlotSel, expected415, &errText),
	)

	trimEqual(t, errText, expected415, "error message")
	if got := counter.get(); got != 1 {
		t.Fatalf("server saw %d POST(s); expected 1", got)
	}
}

// Scenario 5 (WCAG 2.1 AA): the cancel button must be reachable via
// keyboard and pressing it must restore focus to the file input.
//
// We don't drive an in-flight upload (htmx:xhr:progress only fires for
// request-body uploads, which complete instantly for our 77-byte
// fixtures). Instead we exercise the same DOM contract: unhide the
// progress wrapper via the public SinUpload.setProgress helper, then
// Tab from the file input through to the cancel button and verify both
// reachability and post-click focus restoration.
func TestE2E_KeyboardCancelFocusReturn(t *testing.T) {
	srv, _ := startServer(t, postBoom)
	ctx := withBrowser(t)

	var beforeID, afterID string
	var sawCancelText string
	runOrFail(t, ctx, "scenario-5",
		chromedp.Navigate(formPageURL(srv)),
		chromedp.WaitVisible(formSel, chromedp.ByQuery),
		// Force the progress wrapper visible so the cancel button is
		// in the keyboard tab order.
		chromedp.Evaluate(`(function(){
			var f = document.querySelector("form[data-upload='logo']");
			if (!f || !window.SinUpload) return false;
			window.SinUpload.setProgress(f, 1, 100);
			return true;
		})()`, nil),
		chromedp.WaitVisible(cancelBtnSel, chromedp.ByQuery),
		// Focus the file input first — this is where keyboard users
		// land after the label text.
		chromedp.Focus(fileInputSel, chromedp.ByQuery),
		chromedp.Evaluate(`document.activeElement && document.activeElement.id`, &beforeID),
		// Tab forward until we reach the cancel button. There can be a
		// few intermediate focusable elements (preview canvas does NOT
		// take focus, but the progress bar can in some browsers). Loop
		// up to 6 tabs, asserting we land on the cancel button.
		tabUntil(cancelBtnSel, 6),
		chromedp.Text(cancelBtnSel, &sawCancelText, chromedp.ByQuery),
		// Activating the button via Enter must mirror a click.
		chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible(errorSlotSel, chromedp.ByQuery),
		chromedp.Evaluate(`document.activeElement && document.activeElement.id`, &afterID),
	)

	if beforeID != "sin-upload-logo-input" {
		t.Fatalf("focus did not start on file input, got %q", beforeID)
	}
	if got := strings.TrimSpace(sawCancelText); got != "Cancelar" {
		t.Fatalf("cancel button label drift: got %q", got)
	}
	if afterID != "sin-upload-logo-input" {
		t.Fatalf("focus not restored to file input after cancel, got %q", afterID)
	}
}
