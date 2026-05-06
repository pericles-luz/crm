package nomathrand

import (
	"path/filepath"
	"testing"
)

// TestHasGenSuffix_TableDriven covers the filename matcher. The
// convention is "files that mint things end in `_gen.go`, `.gen.go`,
// or are simply `gen.go`", matching the project's existing layout.
func TestHasGenSuffix_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"gen.go", true},
		{"token_gen.go", true},
		{"pkg.gen.go", true},
		{filepath.Join("a", "b", "tokenmint_gen.go"), true},
		{"regen.go", false},
		{"gen_other.go", false},
		{"main.go", false},
		{"", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			got := hasGenSuffix(tc.path, "gen.go")
			if got != tc.want {
				t.Fatalf("hasGenSuffix(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestIsForbidden covers the import-path matcher: the canonical names
// match, sub-packages of math/rand match, but unrelated packages do
// not.
func TestIsForbidden(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"math/rand", true},
		{"math/rand/v2", true},
		{"math/rand/internal", true},
		{"crypto/rand", false},
		{"math", false},
		{"github.com/x/math/rand", false},
		{"", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			got := isForbidden(tc.path)
			if got != tc.want {
				t.Fatalf("isForbidden(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestScopeLabel covers the diagnostic-scope label. The label is what
// the operator sees in the error message, so the table here pins the
// exact strings.
func TestScopeLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		webhook bool
		genFile bool
		want    string
	}{
		{true, true, "webhook + *gen.go scope"},
		{true, false, "webhook scope"},
		{false, true, "*gen.go scope"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := scopeLabel(tc.webhook, tc.genFile); got != tc.want {
				t.Fatalf("scopeLabel(%v,%v) = %q, want %q", tc.webhook, tc.genFile, got, tc.want)
			}
		})
	}
}
