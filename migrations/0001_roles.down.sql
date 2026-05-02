-- 0001_roles.down.sql
-- Reverses 0001_roles.up.sql. Run as superuser. Idempotent.
--
-- Roles can only be dropped when they own no objects in any database. This
-- file therefore assumes that 0002+ have been rolled back first; otherwise
-- DROP OWNED BY would only act on the current database and leave dangling
-- objects elsewhere.

BEGIN;

REVOKE USAGE ON SCHEMA public FROM app_runtime;
REVOKE USAGE ON SCHEMA public FROM app_master_ops;
REVOKE USAGE, CREATE ON SCHEMA public FROM app_admin;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
    EXECUTE 'REASSIGN OWNED BY app_runtime TO CURRENT_USER';
    EXECUTE 'DROP OWNED BY app_runtime';
    DROP ROLE app_runtime;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_master_ops') THEN
    EXECUTE 'REASSIGN OWNED BY app_master_ops TO CURRENT_USER';
    EXECUTE 'DROP OWNED BY app_master_ops';
    DROP ROLE app_master_ops;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_admin') THEN
    EXECUTE 'REASSIGN OWNED BY app_admin TO CURRENT_USER';
    EXECUTE 'DROP OWNED BY app_admin';
    DROP ROLE app_admin;
  END IF;
END $$;

COMMIT;
