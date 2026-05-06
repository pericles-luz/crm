-- 0002_master_ops_audit.up.sql
-- Per-database audit infrastructure for the cross-tenant master_ops role
-- (SIN-62232). Roles are cluster-scoped and live in 0001; this migration
-- creates the audit table and the trigger function in EACH database that
-- holds tenanted tables (one per region/cluster as we scale).
--
-- Run as app_admin (owner of app objects). Idempotent.
--
-- The trigger function is what makes "every master_ops query is audited"
-- a hard guarantee instead of a hope: a missing app.master_ops_actor_user_id
-- GUC raises an exception that aborts the whole transaction.

BEGIN;

-- ---------------------------------------------------------------------------
-- master_ops_audit: append-only ledger of every cross-tenant statement
-- executed under app_master_ops.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS master_ops_audit (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor_user_id   uuid NOT NULL,
  tenant_id       uuid,                   -- NULL = cross-tenant operation
  query_kind      text NOT NULL CHECK (query_kind IN ('insert','update','delete','session_open')),
  target_table    text NOT NULL,
  target_pk       text,
  occurred_at     timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE master_ops_audit OWNER TO app_admin;

-- Append-only: only INSERT for the actor roles, SELECT for both admin/master.
-- Idempotency: REVOKE all then GRANT exactly what we want, in case prior
-- migrations left more permissive grants behind.
REVOKE ALL ON master_ops_audit FROM PUBLIC;
REVOKE ALL ON master_ops_audit FROM app_runtime;
REVOKE ALL ON master_ops_audit FROM app_master_ops;
GRANT INSERT, SELECT ON master_ops_audit TO app_master_ops;
GRANT SELECT ON master_ops_audit TO app_admin;

-- Trigger function: writes one master_ops_audit row per row touched whenever
-- the executing role is app_master_ops. Without app.master_ops_actor_user_id
-- the transaction aborts (this is the load-bearing safety property).
CREATE OR REPLACE FUNCTION master_ops_audit_trigger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_actor uuid;
  v_row   jsonb;
  v_old   jsonb;
BEGIN
  IF current_user <> 'app_master_ops' THEN
    -- runtime/admin paths are not audited here; runtime is constrained by RLS,
    -- admin work happens during deploys and is logged in the deploy runner.
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;

  v_actor := nullif(current_setting('app.master_ops_actor_user_id', true), '')::uuid;
  IF v_actor IS NULL THEN
    RAISE EXCEPTION 'app_master_ops requires app.master_ops_actor_user_id GUC (use WithMasterOps)';
  END IF;

  IF TG_OP = 'DELETE' THEN
    v_row := to_jsonb(OLD);
  ELSE
    v_row := to_jsonb(NEW);
  END IF;
  IF TG_OP = 'UPDATE' THEN
    v_old := to_jsonb(OLD);
  END IF;

  INSERT INTO master_ops_audit (actor_user_id, tenant_id, query_kind, target_table, target_pk)
  VALUES (
    v_actor,
    nullif(coalesce(v_row->>'tenant_id', v_old->>'tenant_id'), '')::uuid,
    LOWER(TG_OP),
    TG_TABLE_NAME,
    coalesce(v_row->>'id', v_old->>'id')
  );

  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

ALTER FUNCTION master_ops_audit_trigger() OWNER TO app_admin;

COMMIT;
