-- 0001_roles.up.sql
-- Bootstrap migration for the multi-tenant CRM (SIN-62232).
--
-- Creates the three Postgres roles documented in
-- docs/adr/0071-postgres-roles.md (app_runtime, app_admin, app_master_ops)
-- and the schema-level grants. CREATE ROLE is cluster-scoped so this
-- migration only runs against ONE database per cluster (typically the
-- maintenance database during the deploy bootstrap).
--
-- THIS FILE MUST BE RUN AS A DATABASE SUPERUSER. Subsequent migrations run
-- as app_admin. CREATE ROLE is a superuser-only operation on most managed
-- providers; we deliberately do not give app_admin CREATEROLE.
--
-- Per-database objects (master_ops_audit, the trigger function) live in
-- 0002_master_ops_audit.up.sql so they can be applied per database without
-- contending on cluster-global pg_authid.

BEGIN;

-- pgcrypto provides gen_random_uuid().
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Roles. Idempotent via DO blocks: re-running this migration must not error.
-- Passwords are deliberately NOT set here; ops injects them at deploy time
-- (see docs/adr/0071-postgres-roles.md "Credential injection").
-- ---------------------------------------------------------------------------

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
    CREATE ROLE app_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  ELSE
    ALTER ROLE app_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_admin') THEN
    -- app_admin needs BYPASSRLS for DDL/seed operations during deploys.
    -- It does NOT have CREATEROLE: only superusers create roles.
    CREATE ROLE app_admin LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  ELSE
    ALTER ROLE app_admin LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_master_ops') THEN
    CREATE ROLE app_master_ops LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  ELSE
    ALTER ROLE app_master_ops LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  END IF;
END $$;

-- Schema-level grants. app_admin owns and creates objects; app_runtime and
-- app_master_ops only USE the schema. Per-table privileges are granted in
-- subsequent migrations.
GRANT USAGE ON SCHEMA public TO app_runtime;
GRANT USAGE ON SCHEMA public TO app_master_ops;
GRANT USAGE, CREATE ON SCHEMA public TO app_admin;

COMMIT;
