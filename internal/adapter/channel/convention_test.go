// Convention test for ADR 0075 rev 3 / F-12 fail-closed sub-rule.
//
// Any ChannelAdapter implementation that returns ok=false from its
// BodyTenantAssociation method bypasses the body↔tenant cross-check.
// That is a SecurityEngineer-reviewed decision and MUST be marked in
// source with the literal token `SecretScope justification:` (in a
// comment) so the marker is greppable, the rationale travels with the
// code, and a regression that adds a silent ok=false path is caught
// here rather than in production.
//
// This test walks every Go file under internal/adapter/channel/. For
// each file containing a BodyTenantAssociation method, if the file
// also contains a literal `false` returned from that method, we
// require the marker to appear somewhere in the same file. The
// convention test is intentionally a textual lint rather than an AST
// one — it has no false-negatives for the simple "snuck in ok=false"
// regression we care about, and it keeps the test self-contained
// (no go/parser, no go/ast).
package channel_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const justificationMarker = "SecretScope justification:"

// matchBodyTenantAssoc captures any line declaring BodyTenantAssociation;
// we check the file rather than the function for the marker, which is
// good enough since the marker convention is "in this file there is at
// least one ok=false path and here is why".
var matchBodyTenantAssoc = regexp.MustCompile(`func\s+\([^)]+\)\s+BodyTenantAssociation\s*\(`)

// matchReturnFalse captures `return ..., false` literal returns. We
// could be more precise with go/ast but the false-positive rate of
// matching e.g. comparisons is acceptable: developers who write
// `boolVar == false` instead of `!boolVar` will trip the lint and add
// the marker, which is harmless.
var matchReturnFalse = regexp.MustCompile(`return\s+[^,]+,\s*false\b`)

func TestBodyTenantAssociation_OkFalseMarkerConvention(t *testing.T) {
	t.Parallel()

	root, err := findChannelRoot()
	if err != nil {
		t.Fatalf("locate channel adapter root: %v", err)
	}

	var failures []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(raw)
		if !matchBodyTenantAssoc.MatchString(src) {
			return nil
		}
		if !matchReturnFalse.MatchString(src) {
			// Adapter has no ok=false path; nothing to justify.
			return nil
		}
		if !strings.Contains(src, justificationMarker) {
			rel, _ := filepath.Rel(root, path)
			failures = append(failures, rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	if len(failures) > 0 {
		t.Fatalf("BodyTenantAssociation in these files returns ok=false but lacks "+
			"`// %s` marker (rev 3 / F-12 fail-closed sub-rule):\n  %s",
			justificationMarker, strings.Join(failures, "\n  "))
	}
}

// findChannelRoot locates internal/adapter/channel relative to wherever
// `go test` was invoked. The package layout puts this _test.go file at
// internal/adapter/channel/, so the package directory itself is the
// root we want to scan. Resolved at runtime so the test works whether
// run from repo root or from this package directory.
func findChannelRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// `go test` runs with the package dir as cwd; that IS the channel
	// root (this file lives there).
	return wd, nil
}
