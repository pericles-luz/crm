//go:build e2e

package upload_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// browserAllocator is the shared chromedp allocator across the suite.
// One Chrome process backs every scenario; each test gets its own
// browser-context (a fresh tab + isolated cookies/storage).
var browserAllocator context.Context

// scenarioTimeout caps how long any one scenario can hold the browser.
// 15s is generous for these tiny pages but still prevents a wedged tab
// from blocking the whole suite forever.
const scenarioTimeout = 15 * time.Second

func TestMain(m *testing.M) {
	if os.Getenv("SIN_E2E_DEBUG") != "" {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
	)
	if os.Getenv("SIN_E2E_HEADFUL") != "" {
		// Drop chromedp.Headless from the upstream defaults so a
		// developer can watch the run. We compare option pointers
		// because ExecAllocatorOption is an opaque func.
		headless := chromedp.Headless
		filtered := opts[:0]
		for _, o := range opts {
			if fmt.Sprintf("%p", o) != fmt.Sprintf("%p", headless) {
				filtered = append(filtered, o)
			}
		}
		opts = filtered
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()
	browserAllocator = allocCtx

	os.Exit(m.Run())
}

// withBrowser builds a fresh tab context for one scenario and tears it
// down when the test ends. The returned context already carries the
// per-scenario timeout so callers can pass it straight into chromedp.Run.
func withBrowser(t *testing.T) context.Context {
	t.Helper()
	if browserAllocator == nil {
		t.Fatal("browserAllocator nil — TestMain was not run; build tag missing?")
	}
	logf := chromedp.WithLogf(func(string, ...any) {})
	errf := chromedp.WithErrorf(func(string, ...any) {})
	debugf := chromedp.WithDebugf(func(string, ...any) {})
	if os.Getenv("SIN_E2E_DEBUG") != "" {
		logf = chromedp.WithLogf(log.Printf)
		errf = chromedp.WithErrorf(log.Printf)
		debugf = chromedp.WithDebugf(log.Printf)
	}
	ctx, cancel := chromedp.NewContext(browserAllocator, logf, errf, debugf)
	t.Cleanup(cancel)
	tctx, tcancel := context.WithTimeout(ctx, scenarioTimeout)
	t.Cleanup(tcancel)
	return tctx
}

// runOrFail wraps chromedp.Run with helpful failure context.
func runOrFail(t *testing.T, ctx context.Context, label string, actions ...chromedp.Action) {
	t.Helper()
	if err := chromedp.Run(ctx, actions...); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: timeout after %s", label, scenarioTimeout)
		}
		t.Fatalf("%s: %v", label, err)
	}
}

// xhrSpyJS hooks XMLHttpRequest.open and window.fetch so each scenario
// can later assert "did htmx fire a POST against /uploads/logo?". The
// spy is installed AFTER navigation (so it lives on the page we are
// inspecting) and BEFORE any user interaction that could trigger a
// request. Idempotent: re-installing on the same page is a no-op.
const xhrSpyJS = `(function(){
  if (window.__sinXHRSpy) return;
  window.__sinXHRSpy = [];
  var realOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function(method, url){
    try { window.__sinXHRSpy.push({method: String(method).toUpperCase(), url: String(url)}); }
    catch(e) {}
    return realOpen.apply(this, arguments);
  };
  var realFetch = window.fetch;
  if (typeof realFetch === "function") {
    window.fetch = function(input, init){
      try {
        var url = typeof input === "string" ? input : (input && input.url) || "";
        var method = (init && init.method) || (input && input.method) || "GET";
        window.__sinXHRSpy.push({method: String(method).toUpperCase(), url: String(url)});
      } catch(e) {}
      return realFetch.apply(this, arguments);
    };
  }
})();`

// xhrCall is one entry from the spy buffer.
type xhrCall struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}
