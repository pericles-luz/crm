// Package upload contains the SIN-62270 end-to-end browser tests for the
// SIN-62258 upload form. Tests are gated behind the "e2e" build tag so
// the regular `go test ./...` suite stays browser-free.
//
// Run with:
//
//	make e2e
//	# or
//	go test -tags=e2e ./internal/e2e/upload/...
//
// Requirements: a working Chrome/Chromium on PATH (driven via the
// chromedp library). See docs/e2e.md for setup, debugging tips, and
// fixture-regeneration instructions.
package upload
