//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/webhook"
)

// TestPartitions_DailyBoundaryCrossing — ADR §4 partition rotation E2E.
//
// raw_event is partitioned daily by received_at. Inserts that cross
// the daily boundary MUST NOT fail with "no partition exists" — the
// app-side cron must have created tomorrow's partition before the
// boundary. We exercise that path by:
//
//  1. Creating tomorrow's and yesterday's partitions via the
//     webhook_create_raw_event_partition function (the same SQL the
//     production cron runs).
//  2. Inserting one row in yesterday's window, one in today's, one
//     in tomorrow's.
//  3. Confirming all three land without error and route to distinct
//     partition tables.
//  4. Dropping yesterday's partition via
//     webhook_drop_raw_event_partition. The DROP must succeed and
//     the row count for the parent table drops accordingly.
func TestPartitions_DailyBoundaryCrossing(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	tomorrow := today.AddDate(0, 0, 1)

	// Ensure yesterday + tomorrow partitions exist (the bootstrap in
	// 0075c only created today + tomorrow; we add yesterday explicitly).
	for _, d := range []time.Time{yesterday, today, tomorrow} {
		if _, err := h.pool.Exec(ctx,
			`SELECT webhook_create_raw_event_partition($1::date)`,
			d.Format("2006-01-02"),
		); err != nil {
			t.Fatalf("create partition for %s: %v", d.Format("2006-01-02"), err)
		}
	}

	tenant := mustParseTenant(t, "fafafafa-fafa-fafa-fafa-fafafafafafa")

	// Insert directly via SQL so we can pin received_at to each window.
	// We use distinct idempotency_keys so the (tenant, channel, key) PK
	// doesn't dedup.
	for i, ts := range []time.Time{
		yesterday.Add(12 * time.Hour),
		today.Add(12 * time.Hour),
		tomorrow.Add(12 * time.Hour),
	} {
		key := []byte(fmt.Sprintf("partition-key-%d", i))
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO raw_event (tenant_id, channel, idempotency_key, raw_payload, headers, received_at)
			 VALUES ($1, 'whatsapp', $2, '{}'::bytea, '{}'::jsonb, $3)`,
			tenant[:], key, ts,
		); err != nil {
			t.Fatalf("insert into partition for %s: %v", ts.Format(time.RFC3339), err)
		}
	}

	// All three rows must be visible from the parent.
	var n int
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM raw_event WHERE tenant_id = $1`,
		tenant[:]).Scan(&n); err != nil {
		t.Fatalf("count parent: %v", err)
	}
	if n != 3 {
		t.Errorf("rows in parent = %d, want 3", n)
	}

	// Drop yesterday's partition via the production SQL function. The
	// row in yesterday's window disappears; the others stay.
	yesterdayPart := fmt.Sprintf("raw_event_%s", yesterday.Format("20060102"))
	if _, err := h.pool.Exec(ctx,
		`SELECT webhook_drop_raw_event_partition($1::text)`, yesterdayPart,
	); err != nil {
		t.Fatalf("drop yesterday partition: %v", err)
	}
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM raw_event WHERE tenant_id = $1`,
		tenant[:]).Scan(&n); err != nil {
		t.Fatalf("count parent after drop: %v", err)
	}
	if n != 2 {
		t.Errorf("rows in parent after drop = %d, want 2 (yesterday gone)", n)
	}

	// Re-running the drop is a no-op (idempotent — the function checks
	// pg_class before EXECUTE).
	if _, err := h.pool.Exec(ctx,
		`SELECT webhook_drop_raw_event_partition($1::text)`, yesterdayPart,
	); err != nil {
		t.Fatalf("re-drop: %v", err)
	}
}

// TestPartitions_HandleAcrossBoundary — exercise the live Service.Handle
// path twice: once with received_at "today", once with the clock advanced
// past midnight UTC. The second call must succeed (tomorrow's partition
// is bootstrapped by 0075c).
func TestPartitions_HandleAcrossBoundary(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	tenant := mustParseTenant(t, "cafecafe-cafe-cafe-cafe-cafecafecafe")
	insertToken(t, h.pool, tenant, "whatsapp", "tok-PB", 0, time.Time{})
	insertAssociation(t, h.pool, tenant, "whatsapp", "PHONE-PB")

	today := time.Now().UTC().Truncate(24 * time.Hour)
	// Use a timestamp solidly inside today's window so the row lands
	// in raw_event_<YYYYMMDD>.
	noonToday := today.Add(12 * time.Hour)
	st := newStack(t, h, noonToday)
	first := signedMetaRequest(t, "whatsapp", "tok-PB", "PHONE-PB", noonToday)
	if r := st.svc.Handle(context.Background(), first); r.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("today: Outcome=%s err=%v", r.Outcome, r.Err)
	}

	// Cross the boundary: clock is set 1 second after midnight UTC on
	// tomorrow's date, payload timestamp follows. Tomorrow's partition
	// was created by the migration's DO $$…$$ block.
	tomorrow := today.Add(24*time.Hour + time.Second)
	st.clock.Set(tomorrow)
	second := signedMetaRequest(t, "whatsapp", "tok-PB", "PHONE-PB", tomorrow)
	if r := st.svc.Handle(context.Background(), second); r.Outcome != webhook.OutcomeAccepted {
		t.Fatalf("tomorrow: Outcome=%s err=%v", r.Outcome, r.Err)
	}

	if got := countRawEvent(t, h.pool, tenant, "whatsapp"); got != 2 {
		t.Errorf("raw_event(today+tomorrow) = %d, want 2", got)
	}
}
