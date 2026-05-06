-- 0075c_raw_event.up.sql — SIN-62234 / ADR 0075 §3 (D7).
-- Storage histórico, NÃO usado para dedup (dedup vive em webhook_idempotency).
-- Particionada por dia em received_at; retenção 30d via DROP PARTITION.
--
-- Estratégia de partição:
--   Optamos por gerenciamento app-side (cron interno em internal/worker/
--   webhook_partition.go) ao invés de pg_partman. Razão: pg_partman exige
--   extensão Postgres (operacional overhead em managed Postgres); cron interno
--   é boring (CREATE TABLE … PARTITION OF … FOR VALUES + DROP TABLE) e auditável
--   no mesmo idioma de outras migrations. Trade-off documentado em ADR 0075 §8
--   ("Particionamento de raw_event exige pg_partman ou cron manual").
--
-- Partições inicial (today + tomorrow) criadas aqui para que o handler nunca
-- caia em "no partition" no primeiro deploy. Cron mantém janela rolling.

CREATE TABLE IF NOT EXISTS raw_event (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL,
    channel         text        NOT NULL,
    idempotency_key bytea       NOT NULL,
    raw_payload     bytea       NOT NULL,
    headers         jsonb       NOT NULL,
    received_at     timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    PRIMARY KEY (received_at, id)
) PARTITION BY RANGE (received_at);

CREATE INDEX IF NOT EXISTS raw_event_unpublished_idx
    ON raw_event (received_at)
    WHERE published_at IS NULL;

-- Bootstrap inicial: today + tomorrow. Cron app-side cria as próximas
-- janelas e dropa as antigas (>30d).
DO $$
DECLARE
    today_start  date := (now() AT TIME ZONE 'UTC')::date;
    today_end    date := today_start + INTERVAL '1 day';
    tomorrow_end date := today_start + INTERVAL '2 days';
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS raw_event_%s PARTITION OF raw_event '
        'FOR VALUES FROM (%L) TO (%L)',
        to_char(today_start, 'YYYYMMDD'),
        today_start,
        today_end
    );
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS raw_event_%s PARTITION OF raw_event '
        'FOR VALUES FROM (%L) TO (%L)',
        to_char(today_end::date, 'YYYYMMDD'),
        today_end,
        tomorrow_end
    );
END $$;
