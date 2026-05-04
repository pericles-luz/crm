package policy_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/ratelimit/policy"
)

const validYAML = `
enabled: true
rules:
  - endpoint: "POST /login"
    bucket: ip
    key: ip
    limit: { window: 1m, max: 5 }
    fail_closed: true
  - endpoint: "POST /login"
    bucket: email
    key: form:email
    limit: { window: 1h, max: 10 }
    fail_closed: true
`

func TestDecode_ValidYAML(t *testing.T) {
	t.Parallel()
	f, err := policy.Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !f.Enabled {
		t.Fatal("enabled must be true")
	}
	if len(f.Rules) != 2 {
		t.Fatalf("rules len = %d, want 2", len(f.Rules))
	}
	r0 := f.Rules[0]
	if r0.Endpoint != "POST /login" || r0.Bucket != "ip" || r0.Key != "ip" {
		t.Fatalf("rule0 mismatch: %+v", r0)
	}
	if r0.Limit.Window != time.Minute || r0.Limit.Max != 5 {
		t.Fatalf("rule0 limit = %+v", r0.Limit)
	}
	if !r0.FailClosed {
		t.Fatal("rule0 fail_closed must be true")
	}
	got := r0.Limit.AsLimit()
	if got.Window != time.Minute || got.Max != 5 {
		t.Fatalf("AsLimit = %+v", got)
	}
}

func TestDecode_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	bad := `
enabled: true
rules:
  - endpoint: "x"
    bucket: y
    key: ip
    limit: { window: 1s, max: 1 }
    bogus_field: 42
`
	_, err := policy.Decode(strings.NewReader(bad))
	if err == nil {
		t.Fatal("Decode must reject unknown fields under KnownFields=true")
	}
}

func TestValidate_RejectsBadRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing endpoint",
			body: `
enabled: true
rules:
  - bucket: ip
    key: ip
    limit: { window: 1s, max: 1 }
`,
			want: "endpoint is required",
		},
		{
			name: "missing bucket",
			body: `
enabled: true
rules:
  - endpoint: "x"
    key: ip
    limit: { window: 1s, max: 1 }
`,
			want: "bucket is required",
		},
		{
			name: "missing key",
			body: `
enabled: true
rules:
  - endpoint: "x"
    bucket: y
    limit: { window: 1s, max: 1 }
`,
			want: "key is required",
		},
		{
			name: "unknown key kind",
			body: `
enabled: true
rules:
  - endpoint: "x"
    bucket: y
    key: cookie:session
    limit: { window: 1s, max: 1 }
`,
			want: "unknown key",
		},
		{
			name: "key needs suffix",
			body: `
enabled: true
rules:
  - endpoint: "x"
    bucket: y
    key: "form:"
    limit: { window: 1s, max: 1 }
`,
			want: "unknown key",
		},
		{
			name: "zero window",
			body: `
enabled: true
rules:
  - endpoint: "x"
    bucket: y
    key: ip
    limit: { window: 0s, max: 1 }
`,
			want: "limit.window",
		},
		{
			name: "zero max",
			body: `
enabled: true
rules:
  - endpoint: "x"
    bucket: y
    key: ip
    limit: { window: 1s, max: 0 }
`,
			want: "limit.max",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := policy.Decode(strings.NewReader(tc.body))
			if err == nil {
				t.Fatal("Decode must surface validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadFile_RoundTripsRepoExample(t *testing.T) {
	t.Parallel()
	// Resolve config/ratelimit.yaml via the module root: the policy package
	// lives at internal/ratelimit/policy, so up four directories.
	pwd, _ := os.Getwd()
	configPath := filepath.Join(pwd, "..", "..", "..", "config", "ratelimit.yaml")
	if _, err := os.Stat(configPath); err != nil {
		t.Skipf("repo example not present at %s (test running outside checkout): %v", configPath, err)
	}
	f, err := policy.LoadFile(configPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !f.Enabled {
		t.Fatal("repo example must default to enabled=true")
	}
	if len(f.Rules) == 0 {
		t.Fatal("repo example must declare at least one rule")
	}
}

func TestLoadFile_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := policy.LoadFile(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("LoadFile must surface missing-file error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadFile error = %v, want wrapping os.ErrNotExist", err)
	}
}
