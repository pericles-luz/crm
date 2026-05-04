//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"
)

// TestMigrations_UpDownUpCycle exercises the rollback path required by
// SIN-62277 acceptance criteria: `up` → `down` → `up` cycle stays green.
// It runs against the harness's existing schema (already migrated up by
// startHarness), rolls everything down, and re-applies up. Each step
// asserts that the canonical objects exist (or are gone, mid-cycle).
func TestMigrations_UpDownUpCycle(t *testing.T) {
	h := startHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// State after harness start: every object should already exist.
	assertObjectsExist(t, ctx, h, true)

	// Roll back. After down, none of the webhook objects survive.
	if err := applyMigrations(ctx, h.pool, h.migrationsDir, "down"); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertObjectsExist(t, ctx, h, false)

	// Re-apply up. Should be idempotent — every CREATE uses IF NOT
	// EXISTS guards (verified by quoting in 0075a..0075d).
	if err := applyMigrations(ctx, h.pool, h.migrationsDir, "up"); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	assertObjectsExist(t, ctx, h, true)
}

// assertObjectsExist verifies the canonical schema objects from
// migrations 0075a..0075d are present (when wantExists==true) or gone.
func assertObjectsExist(t *testing.T, ctx context.Context, h *harness, wantExists bool) {
	t.Helper()
	tables := []string{
		"webhook_tokens",
		"tenant_channel_associations",
		"webhook_idempotency",
		"raw_event",
	}
	for _, name := range tables {
		var exists bool
		row := h.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
			name,
		)
		if err := row.Scan(&exists); err != nil {
			t.Fatalf("scan exists(%s): %v", name, err)
		}
		if exists != wantExists {
			t.Errorf("table %s: exists=%v, want=%v", name, exists, wantExists)
		}
	}

	functions := []string{
		"webhook_gc_idempotency",
		"webhook_drop_raw_event_partition",
		"webhook_create_raw_event_partition",
	}
	for _, name := range functions {
		var exists bool
		row := h.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.proname=$1 AND n.nspname='public')`,
			name,
		)
		if err := row.Scan(&exists); err != nil {
			t.Fatalf("scan exists(fn %s): %v", name, err)
		}
		if exists != wantExists {
			t.Errorf("function %s: exists=%v, want=%v", name, exists, wantExists)
		}
	}
}
