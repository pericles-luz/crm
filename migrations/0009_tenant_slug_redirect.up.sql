-- 0009_tenant_slug_redirect.up.sql — SIN-62244 (F46).
--
-- 12-month redirect window. When a tenant changes slug from `acme` to
-- `acme-2`, the subdomain catch-all consults this table for an active
-- (now() < expires_at) entry on `acme` and answers 301 to
-- `acme-2.<primary>` with `Clear-Site-Data: "cookies"` so any session
-- cookie on the old subdomain is killed before the redirect lands on
-- the new one. Defense-in-depth against subdomain takeover via cookie
-- bleed.
--
-- The reservation table (0008) prevents another tenant from CLAIMING
-- `acme` for 12 months; this table makes sure RETURNING traffic to
-- `acme.<primary>` lands on the new owner of the brand instead of a
-- 404 (or worse, a stranger).
--
-- old_slug is the primary key because at most one active redirect per
-- legacy slug is meaningful — if you re-slug twice in 12 months we
-- update the same row to point at the latest current slug.
--
-- Run as app_admin. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS tenant_slug_redirect (
  old_slug    text        PRIMARY KEY,
  new_slug    text        NOT NULL,
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tenant_slug_redirect_expires_at_idx
  ON tenant_slug_redirect (expires_at);

ALTER TABLE tenant_slug_redirect OWNER TO app_admin;

REVOKE ALL ON tenant_slug_redirect FROM PUBLIC;
REVOKE ALL ON tenant_slug_redirect FROM app_runtime;
REVOKE ALL ON tenant_slug_redirect FROM app_master_ops;
-- Runtime SELECTs at request time on the catch-all handler. Slug-change
-- writes go through master_ops so the redirect creation is audited.
GRANT SELECT ON tenant_slug_redirect TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_slug_redirect TO app_master_ops;

DROP TRIGGER IF EXISTS tenant_slug_redirect_master_ops_audit
  ON tenant_slug_redirect;
CREATE TRIGGER tenant_slug_redirect_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON tenant_slug_redirect
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
