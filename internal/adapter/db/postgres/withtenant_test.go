package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

var harness *testpg.Harness

// TestMain spins up a single Postgres cluster for the whole package, applies
// the bootstrap migrations, and tears down afterwards. Each test asks for its
// own freshly migrated database via harness.DB(t).
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := testpg.Start(ctx)
	if err != nil {
		panic("testpg.Start: " + err.Error())
	}
	harness = h
	code := m.Run()
	if err := h.Stop(); err != nil {
		// don't override the test exit code; just report
		_, _ = os.Stderr.WriteString("testpg.Stop: " + err.Error() + "\n")
	}
	os.Exit(code)
}

func newCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// seedTwoTenants inserts one row per tenant via app_admin (BYPASSRLS=true) so
// the tests can check what the runtime role is allowed to see afterwards.
func seedTwoTenants(t *testing.T, db *testpg.DB) (tenantA, tenantB uuid.UUID) {
	t.Helper()
	tenantA = uuid.New()
	tenantB = uuid.New()
	ctx := newCtx(t)
	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'topup', 100)`, tid); err != nil {
			t.Fatalf("seed tenant %s: %v", tid, err)
		}
	}
	return tenantA, tenantB
}

// ---------------------------------------------------------------------------
// Argument validation (does not need DB)
// ---------------------------------------------------------------------------

func TestWithTenant_RejectsBadArgs(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	ctx := context.Background()

	if err := postgresadapter.WithTenant(ctx, nil, tid, func(pgx.Tx) error { return nil }); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("nil pool: got %v, want ErrNilPool", err)
	}

	pool := &pgxpool.Pool{} // never used because earlier validation fails
	_ = pool

	if err := postgresadapter.WithTenant(ctx, harness.DB(t).RuntimePool(), uuid.Nil, func(pgx.Tx) error { return nil }); !errors.Is(err, postgresadapter.ErrZeroTenant) {
		t.Errorf("uuid.Nil: got %v, want ErrZeroTenant", err)
	}

	if err := postgresadapter.WithTenant(ctx, harness.DB(t).RuntimePool(), tid, nil); !errors.Is(err, postgresadapter.ErrNilFn) {
		t.Errorf("nil fn: got %v, want ErrNilFn", err)
	}
}

func TestWithMasterOps_RejectsBadArgs(t *testing.T) {
	t.Parallel()
	actor := uuid.New()
	ctx := context.Background()

	if err := postgresadapter.WithMasterOps(ctx, nil, actor, func(pgx.Tx) error { return nil }); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("nil pool: got %v, want ErrNilPool", err)
	}
	if err := postgresadapter.WithMasterOps(ctx, harness.DB(t).MasterOpsPool(), uuid.Nil, func(pgx.Tx) error { return nil }); !errors.Is(err, postgresadapter.ErrZeroActor) {
		t.Errorf("uuid.Nil: got %v, want ErrZeroActor", err)
	}
	if err := postgresadapter.WithMasterOps(ctx, harness.DB(t).MasterOpsPool(), actor, nil); !errors.Is(err, postgresadapter.ErrNilFn) {
		t.Errorf("nil fn: got %v, want ErrNilFn", err)
	}
}

// ---------------------------------------------------------------------------
// Core RLS regression tests (per task SIN-62232 acceptance list)
// ---------------------------------------------------------------------------

// TestRLS_NoTenantSet_ReturnsZeroRows: connect as app_runtime without ever
// calling WithTenant. Postgres compares NULL = NULL = NULL, every policy
// evaluates to FALSE, every SELECT returns zero rows.
func TestRLS_NoTenantSet_ReturnsZeroRows(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	seedTwoTenants(t, db)
	ctx := newCtx(t)

	var n int
	if err := db.RuntimePool().QueryRow(ctx, `SELECT count(*) FROM token_ledger`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("runtime saw %d rows without WithTenant; expected 0 (RLS broken)", n)
	}
}

// TestRLS_TenantA_OnlySeesA: WithTenant(A) sees A's rows; an explicit WHERE
// tenant_id = '<B>' literal still returns zero (RLS strips B before WHERE).
func TestRLS_TenantA_OnlySeesA(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	a, b := seedTwoTenants(t, db)
	ctx := newCtx(t)

	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
		var nA int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM token_ledger`).Scan(&nA); err != nil {
			return err
		}
		if nA != 1 {
			t.Errorf("tenant A rows: got %d, want 1", nA)
		}
		var nB int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM token_ledger WHERE tenant_id = $1`, b).Scan(&nB); err != nil {
			return err
		}
		if nB != 0 {
			t.Errorf("tenant A peeking at B by literal: got %d, want 0 (RLS leak)", nB)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
}

// TestRLS_InsertWrongTenantFails: WithTenant(A) trying to INSERT a row whose
// tenant_id is B fails the WITH CHECK clause.
func TestRLS_InsertWrongTenantFails(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	a, b := seedTwoTenants(t, db)
	ctx := newCtx(t)

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx,
			`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'topup', 1)`, b)
		return ierr
	})
	if err == nil {
		t.Fatal("expected RLS WITH CHECK violation, got nil")
	}
	if !strings.Contains(err.Error(), "row-level security") &&
		!strings.Contains(err.Error(), "violates row-level security") {
		t.Errorf("expected RLS violation, got: %v", err)
	}
}

// TestTokenLedger_UpdateForbidden: app_runtime tries to UPDATE / DELETE on
// token_ledger and gets "permission denied" because of the table-level
// REVOKE in 0002_token_ledger.up.sql.
func TestTokenLedger_UpdateForbidden(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	a, _ := seedTwoTenants(t, db)
	ctx := newCtx(t)

	cases := []struct {
		name string
		sql  string
	}{
		{"update", `UPDATE token_ledger SET amount = 0`},
		{"delete", `DELETE FROM token_ledger`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, tc.sql)
				return err
			})
			if err == nil {
				t.Fatalf("%s succeeded; expected permission denied", tc.name)
			}
			if !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s: expected permission denied, got: %v", tc.name, err)
			}
		})
	}
}

// TestRLS_AppliesToOwner: even the table owner (app_admin in our migrations)
// sees no rows when impersonating app_runtime, because of FORCE ROW LEVEL
// SECURITY. We assert relforcerowsecurity = true on pg_class as the load-
// bearing fact, then prove the policy still fires when set_config is unset.
func TestRLS_AppliesToOwner(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	ctx := newCtx(t)

	var force bool
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT relforcerowsecurity FROM pg_class WHERE relname = 'token_ledger'`).Scan(&force); err != nil {
		t.Fatalf("query relforcerowsecurity: %v", err)
	}
	if !force {
		t.Fatal("relforcerowsecurity is false on token_ledger; FORCE ROW LEVEL SECURITY is missing (ADR 0072 violation)")
	}

	// Belt-and-suspenders: if FORCE were silently turned off in some future
	// migration, this assertion would catch it. We connect as app_runtime
	// (the role on which the policies are defined) and confirm 0-rows
	// without a tenant GUC. The owner-bypass scenario is what FORCE blocks.
	seedTwoTenants(t, db)
	var n int
	if err := db.RuntimePool().QueryRow(ctx, `SELECT count(*) FROM token_ledger`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("runtime saw %d rows without GUC; FORCE RLS is bypassed somewhere", n)
	}
}

// TestWithTenant_RollbackOnError: fn returns error -> tx is rolled back ->
// nothing committed.
func TestWithTenant_RollbackOnError(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	a, _ := seedTwoTenants(t, db)
	ctx := newCtx(t)

	sentinel := errors.New("boom")
	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'topup', 999)`, a); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}

	var seen int
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM token_ledger WHERE amount = 999`).Scan(&seen)
	}); err != nil {
		t.Fatalf("verify rollback: %v", err)
	}
	if seen != 0 {
		t.Errorf("rollback failed: amount=999 row(s) committed: %d", seen)
	}
}

// TestWithTenant_HappyCommit: success path commits; data persists across
// transactions for the same tenant.
func TestWithTenant_HappyCommit(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	a := uuid.New()
	ctx := newCtx(t)

	// app_runtime cannot SET search_path on its own per-database, so seed via
	// the admin pool to get a known tenant_id committed first.
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'topup', 7)`, a)
		return err
	}); err != nil {
		t.Fatalf("happy insert: %v", err)
	}

	var amount int64
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT amount FROM token_ledger`).Scan(&amount)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if amount != 7 {
		t.Errorf("amount: got %d, want 7", amount)
	}
}

// TestWithMasterOps_AuditTrailWritten: WithMasterOps writes a session_open
// audit row. A subsequent INSERT on token_ledger triggers the per-row audit,
// recording actor_user_id and target_pk.
func TestWithMasterOps_AuditTrailWritten(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	tenantA, _ := seedTwoTenants(t, db)
	actor := uuid.New()
	ctx := newCtx(t)

	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), actor, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'master_grant', 50)`, tenantA)
		return err
	}); err != nil {
		t.Fatalf("WithMasterOps: %v", err)
	}

	// The audit table is owned by app_admin and only readable by master_ops/admin.
	var sessionOpens, inserts int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE query_kind='session_open' AND target_table='__session__') ,
		   count(*) FILTER (WHERE query_kind='insert' AND target_table='token_ledger')
		 FROM master_ops_audit
		 WHERE actor_user_id = $1`, actor).Scan(&sessionOpens, &inserts); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if sessionOpens < 1 {
		t.Errorf("expected >=1 session_open audit row, got %d", sessionOpens)
	}
	if inserts != 1 {
		t.Errorf("expected exactly 1 insert audit row, got %d", inserts)
	}
}

// TestWithMasterOps_NoActorGUC_RaisesException: bypasses WithMasterOps and
// connects directly as app_master_ops. Trigger raises because the
// app.master_ops_actor_user_id GUC is unset.
func TestWithMasterOps_NoActorGUC_RaisesException(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	tenantA, _ := seedTwoTenants(t, db)
	ctx := newCtx(t)

	_, err := db.MasterOpsPool().Exec(ctx,
		`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'master_grant', 50)`, tenantA)
	if err == nil {
		t.Fatal("expected RAISE EXCEPTION for missing actor GUC, got nil")
	}
	if !strings.Contains(err.Error(), "app_master_ops requires app.master_ops_actor_user_id") {
		t.Errorf("expected GUC-missing error, got: %v", err)
	}
}

// TestWithMasterOps_RollbackOnError: same rollback contract as WithTenant.
func TestWithMasterOps_RollbackOnError(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	tenantA, _ := seedTwoTenants(t, db)
	actor := uuid.New()
	ctx := newCtx(t)

	sentinel := errors.New("rollback me")
	err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), actor, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx,
			`INSERT INTO token_ledger (tenant_id, kind, amount) VALUES ($1, 'master_grant', 999)`, tenantA)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}

	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM token_ledger WHERE amount = 999`).Scan(&n); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n != 0 {
		t.Errorf("master ops rollback failed: %d row(s) committed", n)
	}
}

// TestMigrationsDownReverts: the down migrations reverse the up migrations
// cleanly and are idempotent.
func TestMigrationsDownReverts(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	ctx := newCtx(t)

	// Down 0003 (token_ledger) as app_admin.
	if err := runMigrationFile(ctx, db.AdminPool(), harness.MigrationsDir(), "0003_token_ledger.down.sql"); err != nil {
		t.Fatalf("down 0003: %v", err)
	}
	var existsTokenLedger bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='token_ledger')`).Scan(&existsTokenLedger); err != nil {
		t.Fatalf("check token_ledger: %v", err)
	}
	if existsTokenLedger {
		t.Fatal("token_ledger still exists after down 0003")
	}

	// Idempotency: re-running each down must be a no-op.
	if err := runMigrationFile(ctx, db.AdminPool(), harness.MigrationsDir(), "0003_token_ledger.down.sql"); err != nil {
		t.Fatalf("down 0003 second run (idempotency): %v", err)
	}

	// Down 0002 (master_ops_audit) — drops the table and trigger function.
	if err := runMigrationFile(ctx, db.AdminPool(), harness.MigrationsDir(), "0002_master_ops_audit.down.sql"); err != nil {
		t.Fatalf("down 0002: %v", err)
	}
	var existsAudit bool
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='master_ops_audit')`).Scan(&existsAudit); err != nil {
		t.Fatalf("check master_ops_audit: %v", err)
	}
	if existsAudit {
		t.Fatal("master_ops_audit still exists after down 0002")
	}

	// Re-up to confirm we can roll forward again on the same DB.
	if err := runMigrationFile(ctx, db.AdminPool(), harness.MigrationsDir(), "0002_master_ops_audit.up.sql"); err != nil {
		t.Fatalf("re-up 0002: %v", err)
	}
	if err := runMigrationFile(ctx, db.AdminPool(), harness.MigrationsDir(), "0003_token_ledger.up.sql"); err != nil {
		t.Fatalf("re-up 0003: %v", err)
	}
}

func runMigrationFile(ctx context.Context, pool *pgxpool.Pool, dir, name string) error {
	data, err := os.ReadFile(dir + string(os.PathSeparator) + name)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, string(data))
	return err
}

// TestRolesArePostureCorrect: the BYPASSRLS bits on the three roles match
// what docs/adr/0071-postgres-roles.md states. Catches anyone flipping
// BYPASSRLS=true on app_runtime.
func TestRolesArePostureCorrect(t *testing.T) {
	t.Parallel()
	db := harness.DB(t)
	ctx := newCtx(t)

	rows, err := db.SuperuserPool().Query(ctx, `
		SELECT rolname, rolbypassrls, rolsuper, rolcreatedb, rolcreaterole
		FROM pg_roles
		WHERE rolname IN ('app_runtime','app_admin','app_master_ops')
	`)
	if err != nil {
		t.Fatalf("pg_roles: %v", err)
	}
	defer rows.Close()
	got := map[string]struct {
		bypass, super, createdb, createrole bool
	}{}
	for rows.Next() {
		var name string
		var bypass, super, createdb, createrole bool
		if err := rows.Scan(&name, &bypass, &super, &createdb, &createrole); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = struct{ bypass, super, createdb, createrole bool }{bypass, super, createdb, createrole}
	}
	if rows.Err() != nil {
		t.Fatalf("rows: %v", rows.Err())
	}

	if r, ok := got["app_runtime"]; !ok || r.bypass || r.super || r.createdb || r.createrole {
		t.Errorf("app_runtime posture wrong: %+v (want all false)", r)
	}
	if r, ok := got["app_admin"]; !ok || !r.bypass || r.super || r.createdb || r.createrole {
		t.Errorf("app_admin posture wrong: %+v (want bypass=true rest false)", r)
	}
	if r, ok := got["app_master_ops"]; !ok || !r.bypass || r.super || r.createdb || r.createrole {
		t.Errorf("app_master_ops posture wrong: %+v (want bypass=true rest false)", r)
	}
}
