package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/webhook"
)

// TestRealMain_MissingDSN ensures the CLI fails fast and explicitly
// when neither --dsn nor the env-resolver returns a string.
func TestRealMain_MissingDSN(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := realMain(
		[]string{"--channel", "whatsapp", "--tenant-id", "00000000-0000-0000-0000-000000000001"},
		&stdout, &stderr,
		func(string) string { return "" },
		func(context.Context, string) (webhook.TokenAdmin, func(), error) {
			t.Fatal("buildAdmin must NOT be called when DSN is missing")
			return nil, nil, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v, want missing-DSN error", err)
	}
}

// TestRealMain_BadFlag returns the parser's error verbatim so the
// operator gets a non-zero exit code and the standard "flag provided
// but not defined" message.
func TestRealMain_BadFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := realMain(
		[]string{"--bogus"},
		&stdout, &stderr,
		func(string) string { return "x" },
		func(context.Context, string) (webhook.TokenAdmin, func(), error) {
			t.Fatal("buildAdmin must NOT be called when flag parsing fails")
			return nil, nil, nil
		},
	)
	if err == nil {
		t.Fatal("expected flag parsing error")
	}
}

// TestRealMain_BuildAdminFails shows realMain surfaces the buildAdmin
// error to the caller (so main() can print it before exit 1).
func TestRealMain_BuildAdminFails(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("dial: connection refused")
	var stdout, stderr bytes.Buffer
	err := realMain(
		[]string{"--channel", "whatsapp", "--tenant-id", "00000000-0000-0000-0000-000000000001"},
		&stdout, &stderr,
		func(string) string { return "postgres://nowhere" },
		func(context.Context, string) (webhook.TokenAdmin, func(), error) {
			return nil, nil, wantErr
		},
	)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestRealMain_HappyPath proves realMain wires its flags through to
// Options correctly: the buildAdmin returns a fakeAdmin, Run executes,
// the row lands, and the closer fires exactly once.
func TestRealMain_HappyPath(t *testing.T) {
	t.Parallel()
	admin := &fakeAdmin{}
	closes := 0
	closer := func() { closes++ }

	var stdout, stderr bytes.Buffer
	err := realMain(
		[]string{
			"--channel", "whatsapp",
			"--tenant-id", "11111111-2222-3333-4444-555555555555",
			"--overlap-minutes", "0",
		},
		&stdout, &stderr,
		func(string) string { return "postgres://x" },
		func(context.Context, string) (webhook.TokenAdmin, func(), error) {
			return admin, closer, nil
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(admin.rows); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
	if closes != 1 {
		t.Fatalf("closer fired %d times, want 1", closes)
	}
	if !strings.Contains(stdout.String(), "TOKEN PLAINTEXT") {
		t.Fatalf("missing token banner in stdout:\n%s", stdout.String())
	}
}

// TestDefaultResolveDSN_FlagWins covers the precedence rule: --dsn
// flag value beats DATABASE_URL when both are set.
func TestDefaultResolveDSN_FlagWins(t *testing.T) {
	t.Setenv("DATABASE_URL", "from-env")
	if got := defaultResolveDSN("from-flag"); got != "from-flag" {
		t.Fatalf("got %q, want from-flag", got)
	}
}

// TestDefaultResolveDSN_EnvFallback covers the fallback rule: empty
// flag → env var.
func TestDefaultResolveDSN_EnvFallback(t *testing.T) {
	t.Setenv("DATABASE_URL", "from-env")
	if got := defaultResolveDSN(""); got != "from-env" {
		t.Fatalf("got %q, want from-env", got)
	}
}

// TestDefaultResolveDSN_EmptyBoth covers the missing-DSN case.
func TestDefaultResolveDSN_EmptyBoth(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if got := defaultResolveDSN(""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestDefaultBuildAdmin_BadDSN exercises the production builder's
// failure path with an unparseable DSN. We do not exercise the happy
// path here because that would require a real Postgres reachable from
// the test env — that path is covered by the integration job in CI.
func TestDefaultBuildAdmin_BadDSN(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := defaultBuildAdmin(ctx, "this is not a valid dsn")
	if err == nil {
		t.Fatal("expected error on bad DSN")
	}
}
