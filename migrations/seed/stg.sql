-- migrations/seed/stg.sql
-- Staging seed (SIN-62209). Two tenants (acme, globex), one agent user
-- per tenant, and one global master user. Idempotent: ON CONFLICT clauses
-- let `make seed-stg` run repeatedly without erroring.
--
-- Run as app_admin (BYPASSRLS) so policies do not block the inserts.
--
-- Password hashes are bcrypt cost 4 of the literal string "stg-password" —
-- adequate for staging fixtures, NEVER for prod. Keep this file
-- staging-only; the prod seed lives in PR9 and is sourced from secrets.

BEGIN;

-- Stable UUIDs so callers can reference them in scripts/tests.
INSERT INTO tenants (id, name, host) VALUES
  ('00000000-0000-0000-0000-00000000ac01', 'acme',   'acme.crm.local'),
  ('00000000-0000-0000-0000-00000000eb02', 'globex', 'globex.crm.local')
ON CONFLICT (id) DO NOTHING;

INSERT INTO users (id, tenant_id, email, password_hash, role, is_master) VALUES
  ('00000000-0000-0000-0000-0000000a0e01',
   '00000000-0000-0000-0000-00000000ac01',
   'agent@acme.crm.local',
   '$2a$04$wHy3bTk0jS8eQ5G6wY1uMOZjhqGn0xj2mA4P0vYHt1nQd2u4ZJWne',
   'agent', false),
  ('00000000-0000-0000-0000-0000000e0e02',
   '00000000-0000-0000-0000-00000000eb02',
   'agent@globex.crm.local',
   '$2a$04$wHy3bTk0jS8eQ5G6wY1uMOZjhqGn0xj2mA4P0vYHt1nQd2u4ZJWne',
   'agent', false),
  ('00000000-0000-0000-0000-0000000a57e7',
   NULL,
   'master@crm.local',
   '$2a$04$wHy3bTk0jS8eQ5G6wY1uMOZjhqGn0xj2mA4P0vYHt1nQd2u4ZJWne',
   'master', true)
ON CONFLICT (id) DO NOTHING;

COMMIT;
