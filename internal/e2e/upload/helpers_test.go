//go:build e2e

package upload_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// namedAction is a chromedp.Action with a label, so step-by-step
// runners can identify the exact step that failed without dumping the
// full chromedp call site.
type namedAction struct {
	name   string
	action chromedp.Action
}

// runSteps executes each step in turn, surfacing both the step name and
// the underlying error on failure. Compared to one big chromedp.Run,
// this trades a tiny bit of RPC overhead for far better diagnostics.
//
// On failure it also dumps the current page (URL + outerHTML + spy
// buffer) so a CI log alone is enough to reconstruct what went wrong.
func runSteps(t *testing.T, ctx context.Context, label string, steps []namedAction) {
	t.Helper()
	for _, s := range steps {
		if err := chromedp.Run(ctx, s.action); err != nil {
			dumpPage(t, ctx)
			t.Fatalf("%s: step %q failed: %v", label, s.name, err)
		}
	}
}

// hasUploadPost returns true if the spy buffer contains a POST whose URL
// references the upload action (substring match — httptest assigns a
// random host:port, so we anchor on the path).
func hasUploadPost(calls []xhrCall) bool {
	for _, c := range calls {
		if c.Method == "POST" && strings.Contains(c.URL, uploadAction) {
			return true
		}
	}
	return false
}

// waitClassifySettled polls until upload.js's change-handler classify()
// has clearly resolved. Two terminal states are observable from the DOM:
//
//   - rejected → upload.js cleared input.value (input.files is now empty)
//   - accepted → input still has its file AND ~150ms have elapsed (giving
//     the FileReader-backed classify() time to run)
//
// The 150ms quiescence window is generous: classify reads only 12 bytes,
// so the .then callback typically runs in single-digit milliseconds. We
// pad it to absorb FileReader-task scheduling jitter on slow CI hosts.
//
// The preview canvas is NOT a reliable "accepted" signal: img.onerror
// fires when the underlying File's MIME type (derived from the .svg or
// .png file extension) disagrees with the magic-byte format.
func waitClassifySettled(inputSelector string) chromedp.Action {
	const stabilityWindow = 150 * time.Millisecond
	expr := fmt.Sprintf(`(function(){
		var i = document.querySelector(%q);
		if (!i) return -1;
		return i.files ? i.files.length : 0;
	})()`, inputSelector)
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(scenarioTimeout)
		}
		stableSince := time.Time{}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			var n int
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &n)); err != nil {
				return fmt.Errorf("waitClassifySettled eval: %w", err)
			}
			switch {
			case n == 0:
				// Rejected: classify cleared input.value — settled.
				return nil
			case n > 0:
				if stableSince.IsZero() {
					stableSince = time.Now()
				} else if time.Since(stableSince) >= stabilityWindow {
					return nil
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("waitClassifySettled: timed out (last files=%d)", n)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
}

// assertJSTrue evaluates expr (which must produce a boolean) and fails
// the action if it is not true. message is included in the error.
func assertJSTrue(expr, message string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
			return fmt.Errorf("assertJSTrue eval: %w", err)
		}
		if !ok {
			return fmt.Errorf("assertJSTrue: %s", message)
		}
		return nil
	})
}

// waitErrorEquals polls the error-slot text until it equals want or the
// chromedp context expires. Avoids a Sleep — htmx fires the response
// asynchronously and the error fill happens in a microtask after that.
//
// The text is read once at the end and copied into got so the caller can
// log the actual value on assertion mismatch.
func waitErrorEquals(selector, want string, got *string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(scenarioTimeout)
		}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			var have string
			if err := chromedp.Run(ctx,
				chromedp.WaitVisible(selector, chromedp.ByQuery),
				chromedp.Text(selector, &have, chromedp.ByQuery),
			); err != nil {
				return fmt.Errorf("waitErrorEquals(%s): %w", selector, err)
			}
			if strings.TrimSpace(have) == want {
				if got != nil {
					*got = have
				}
				return nil
			}
			if time.Now().After(deadline) {
				if got != nil {
					*got = have
				}
				return fmt.Errorf("waitErrorEquals(%s): timed out waiting for %q (last %q)", selector, want, have)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
}

// waitHTMXReady polls the page until window.htmx is defined and the
// upload form has been processed. htmx attaches its handlers during
// DOMContentLoaded; under chromedp the navigation can complete BEFORE
// htmx has finished binding, which lets a too-eager Click race past
// hx-post and submit the form natively.
func waitHTMXReady() chromedp.Action {
	const expr = `(typeof window.htmx === "object" && window.htmx !== null
		&& document.querySelector("form[data-upload]") !== null)`
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(scenarioTimeout)
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			var ready bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ready)); err != nil {
				return fmt.Errorf("waitHTMXReady eval: %w", err)
			}
			if ready {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("waitHTMXReady: timed out before window.htmx was defined")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
}

// waitServerPOST polls the per-test counter until it reaches want or
// the chromedp context expires. Useful when the post-swap selector also
// depends on the request reaching the server.
func waitServerPOST(c *postCounter, want int64) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(scenarioTimeout)
		}
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			if c.get() >= want {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("waitServerPOST: server saw %d POST(s); wanted %d", c.get(), want)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
}

// dumpPage logs the current page HTML and any pending console errors.
// Best-effort: if anything fails we just bail silently — the caller is
// already on the failure path.
func dumpPage(t *testing.T, ctx context.Context) {
	t.Helper()
	dumpCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Reuse the existing browser context for evaluation but cap the time
	// since the original ctx may already be cancelled.
	_ = dumpCtx
	var html, urlStr, errors string
	if err := chromedp.Run(ctx,
		chromedp.Location(&urlStr),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
		chromedp.Evaluate(`JSON.stringify(window.__sinXHRSpy || [])`, &errors),
	); err != nil {
		t.Logf("dumpPage: %v", err)
		return
	}
	t.Logf("dumpPage url=%s\nspy=%s\nhtml=%s", urlStr, errors, html)
}

// tabUntil presses Tab up to maxPresses times and stops once
// document.activeElement matches the given selector. Errors out (rather
// than silently overshooting) so a tab-order regression surfaces clearly.
func tabUntil(selector string, maxPresses int) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		expr := fmt.Sprintf(`(function(){
			var el = document.activeElement;
			if (!el) return false;
			return el.matches(%q);
		})()`, selector)
		for i := 0; i < maxPresses; i++ {
			var matched bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &matched)); err != nil {
				return fmt.Errorf("tabUntil eval: %w", err)
			}
			if matched {
				return nil
			}
			if err := chromedp.Run(ctx, chromedp.KeyEvent(kb.Tab)); err != nil {
				return fmt.Errorf("tabUntil tab: %w", err)
			}
		}
		// Final check after the last Tab.
		var matched bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &matched)); err != nil {
			return fmt.Errorf("tabUntil final eval: %w", err)
		}
		if !matched {
			return fmt.Errorf("tabUntil(%s): did not reach selector after %d Tab(s)", selector, maxPresses)
		}
		return nil
	})
}
