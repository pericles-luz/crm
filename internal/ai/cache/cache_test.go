package cache_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/ai/cache"
)

func TestTenantKey_RejectsBadInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		tenantID string
		conv     string
		untilMsg string
		wantErr  error
	}{
		{"empty tenant", "", "conv-1", "msg-1", cache.ErrEmptyTenant},
		{"system literal tenant", "system", "conv-1", "msg-1", cache.ErrSystemTenantMisuse},
		{"zero UUID tenant", "00000000-0000-0000-0000-000000000000", "conv-1", "msg-1", cache.ErrSystemTenantMisuse},
		{"empty conv", "tenant-a", "", "msg-1", cache.ErrEmptyConv},
		{"empty untilMsg", "tenant-a", "conv-1", "", cache.ErrEmptyUntilMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cache.TenantKey(tc.tenantID, tc.conv, tc.untilMsg)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("TenantKey(%q,%q,%q) error = %v, want %v", tc.tenantID, tc.conv, tc.untilMsg, err, tc.wantErr)
			}
			if !got.IsZero() {
				t.Fatalf("TenantKey returned non-zero Key %q on error", got.String())
			}
		})
	}
}

func TestTenantKey_DistinguishesAllThreeBadTenantInputs(t *testing.T) {
	t.Parallel()
	if _, err := cache.TenantKey("", "c", "m"); !errors.Is(err, cache.ErrEmptyTenant) {
		t.Fatalf("empty tenant error = %v, want ErrEmptyTenant", err)
	}
	if _, err := cache.TenantKey("system", "c", "m"); !errors.Is(err, cache.ErrSystemTenantMisuse) {
		t.Fatalf("system tenant error = %v, want ErrSystemTenantMisuse", err)
	}
	if _, err := cache.TenantKey("00000000-0000-0000-0000-000000000000", "c", "m"); !errors.Is(err, cache.ErrSystemTenantMisuse) {
		t.Fatalf("zero UUID error = %v, want ErrSystemTenantMisuse", err)
	}
	if errors.Is(cache.ErrEmptyTenant, cache.ErrSystemTenantMisuse) {
		t.Fatal("ErrEmptyTenant must not be Is-equivalent to ErrSystemTenantMisuse")
	}
}

func TestTenantKey_HappyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tenantID string
		conv     string
		untilMsg string
		want     string
	}{
		{
			name:     "uuid tenant",
			tenantID: "11111111-2222-3333-4444-555555555555",
			conv:     "conv-7",
			untilMsg: "msg-42",
			want:     "tenant:11111111-2222-3333-4444-555555555555:ai:summary:conv-7:msg-42",
		},
		{
			name:     "human-readable tenant",
			tenantID: "acme",
			conv:     "C-1",
			untilMsg: "M-1",
			want:     "tenant:acme:ai:summary:C-1:M-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cache.TenantKey(tc.tenantID, tc.conv, tc.untilMsg)
			if err != nil {
				t.Fatalf("TenantKey: %v", err)
			}
			if got.IsZero() {
				t.Fatal("TenantKey returned zero Key on success")
			}
			if got.String() != tc.want {
				t.Fatalf("TenantKey = %q, want %q", got.String(), tc.want)
			}
			if !strings.HasPrefix(got.String(), "tenant:") {
				t.Fatalf("tenant key %q missing tenant: prefix", got.String())
			}
		})
	}
}

func TestSystemKey_HappyPath(t *testing.T) {
	t.Parallel()
	got, err := cache.SystemKey("summary", "conv-1", "msg-1")
	if err != nil {
		t.Fatalf("SystemKey: %v", err)
	}
	if got.String() != "system:ai:summary:conv-1:msg-1" {
		t.Fatalf("SystemKey = %q", got.String())
	}
}

func TestSystemKey_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		scope    string
		conv     string
		untilMsg string
		wantErr  error
	}{
		{"empty scope", "", "conv-1", "msg-1", cache.ErrEmptyScope},
		{"system scope", "system", "conv-1", "msg-1", cache.ErrSystemScopeMisuse},
		{"empty conv", "summary", "", "msg-1", cache.ErrEmptyConv},
		{"empty untilMsg", "summary", "conv-1", "", cache.ErrEmptyUntilMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cache.SystemKey(tc.scope, tc.conv, tc.untilMsg)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SystemKey(%q,%q,%q) error = %v, want %v", tc.scope, tc.conv, tc.untilMsg, err, tc.wantErr)
			}
			if !got.IsZero() {
				t.Fatalf("SystemKey returned non-zero Key %q on error", got.String())
			}
		})
	}
}

func TestNamespacesAreDisjoint(t *testing.T) {
	t.Parallel()
	tenant, err := cache.TenantKey("acme", "conv-1", "msg-1")
	if err != nil {
		t.Fatalf("TenantKey: %v", err)
	}
	sys, err := cache.SystemKey("summary", "conv-1", "msg-1")
	if err != nil {
		t.Fatalf("SystemKey: %v", err)
	}
	if tenant.String() == sys.String() {
		t.Fatalf("tenant and system keys collided: %q", tenant.String())
	}
	if strings.HasPrefix(tenant.String(), "system:") {
		t.Fatalf("tenant key %q must not start with system:", tenant.String())
	}
	if strings.HasPrefix(sys.String(), "tenant:") {
		t.Fatalf("system key %q must not start with tenant:", sys.String())
	}
}

func TestNoSystemTenantCanCollideWithSystemNamespace(t *testing.T) {
	t.Parallel()
	// A tenant id that is "ai" plus a scope segment would otherwise produce a
	// string identical to "tenant:ai:summary:conv:msg" — but the system
	// namespace begins with "system:ai:", so no tenant id can ever shape a key
	// that starts with "system:" because we always prepend "tenant:".
	tenant, err := cache.TenantKey("system", "conv-1", "msg-1")
	if err == nil {
		t.Fatalf("TenantKey accepted reserved system tenant: %q", tenant.String())
	}
}

func TestKeyZeroValueIsZero(t *testing.T) {
	t.Parallel()
	var k cache.Key
	if !k.IsZero() {
		t.Fatal("zero-value Key must report IsZero")
	}
	if k.String() != "" {
		t.Fatalf("zero-value Key.String = %q, want empty", k.String())
	}
}

func TestNonZeroKeyIsNotZero(t *testing.T) {
	t.Parallel()
	k, err := cache.TenantKey("acme", "c", "m")
	if err != nil {
		t.Fatalf("TenantKey: %v", err)
	}
	if k.IsZero() {
		t.Fatal("constructed Key must not report IsZero")
	}
}
