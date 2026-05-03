-- 0075c_raw_event.down.sql — SIN-62234 / ADR 0075 §6 rollback.
-- DROP TABLE da parent solta as partições filhas em cascata.
DROP INDEX IF EXISTS raw_event_unpublished_idx;
DROP TABLE IF EXISTS raw_event;
