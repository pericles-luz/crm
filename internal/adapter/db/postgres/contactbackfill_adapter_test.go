package postgres_test

// Integration tests for internal/adapter/db/postgres/contactbackfill.
//
// Lives in the parent postgres_test package rather than
// db/postgres/contactbackfill_test, for the same reason
// contacts_adapter_test.go does (see its file doc comment): every test
// binary that calls testpg.Start() bootstraps the SHARED Postgres
// cluster by ALTERing the app_admin/app_runtime/app_master_ops role
// passwords, and two binaries racing that ALTER in parallel
// (`go test ./...`) fail with SQLSTATE 28P01. Keeping the tests here
// means this package shares the one TestMain/harness with every other
// db/postgres/*_test.go file instead of starting a second cluster.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/contactbackfill"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// seedBackfillContact inserts a contact + a single channel identity for
// it, bypassing RLS via AdminPool (the same seeding style
// seedContactsTenant's callers use elsewhere in this package).
func seedBackfillContact(t *testing.T, db *testpg.DB, tenantID uuid.UUID, displayName, channel, externalID string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	contactID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, $3)`,
		contactID, tenantID, displayName); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id) VALUES ($1, $2, $3, $4)`,
		tenantID, contactID, channel, externalID); err != nil {
		t.Fatalf("seed contact_channel_identity: %v", err)
	}
	return contactID
}

func TestContactBackfillAdapter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := contactbackfill.New(nil); err == nil {
		t.Fatal("expected error for nil pool")
	}
}

func TestContactBackfillAdapter_ListCandidates_FindsUnnamedAcrossTenants(t *testing.T) {
	t.Parallel()
	db := freshDBWithInboxContacts(t)
	store, err := contactbackfill.New(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenantA := seedContactsTenant(t, db)
	tenantB := seedContactsTenant(t, db)

	unnamedMessenger := seedBackfillContact(t, db, tenantA, "psid-unnamed", "messenger", "psid-unnamed")
	unnamedInstagram := seedBackfillContact(t, db, tenantB, "igsid-unnamed", "instagram", "igsid-unnamed")
	// Already has a real name — must NOT be returned.
	seedBackfillContact(t, db, tenantA, "Maria Silva", "messenger", "psid-named")
	// A different channel entirely — must NOT be returned even though
	// display_name happens to equal external_id (webchat has no such
	// gap; this pins the query's explicit channel filter).
	seedBackfillContact(t, db, tenantA, "webchat-raw-id", "webchat", "webchat-raw-id")

	got, err := store.ListCandidates(context.Background(), "")
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	ids := map[uuid.UUID]contactbackfill.Candidate{}
	for _, c := range got {
		ids[c.ContactID] = c
	}
	if _, ok := ids[unnamedMessenger]; !ok {
		t.Errorf("expected messenger candidate %s in result", unnamedMessenger)
	}
	if _, ok := ids[unnamedInstagram]; !ok {
		t.Errorf("expected instagram candidate %s in result", unnamedInstagram)
	}
	if len(ids) != 2 {
		t.Errorf("expected exactly 2 candidates, got %d: %+v", len(ids), got)
	}
}

func TestContactBackfillAdapter_ListCandidates_ChannelFilter(t *testing.T) {
	t.Parallel()
	db := freshDBWithInboxContacts(t)
	store, err := contactbackfill.New(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenant := seedContactsTenant(t, db)
	seedBackfillContact(t, db, tenant, "psid-x", "messenger", "psid-x")
	seedBackfillContact(t, db, tenant, "igsid-x", "instagram", "igsid-x")

	got, err := store.ListCandidates(context.Background(), "instagram")
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Channel != "instagram" {
		t.Fatalf("expected exactly 1 instagram candidate, got %+v", got)
	}
}

func TestContactBackfillAdapter_UpdateDisplayName_AppliesWhenGuardMatches(t *testing.T) {
	t.Parallel()
	db := freshDBWithInboxContacts(t)
	store, err := contactbackfill.New(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenant := seedContactsTenant(t, db)
	contactID := seedBackfillContact(t, db, tenant, "psid-1", "messenger", "psid-1")

	updated, err := store.UpdateDisplayName(context.Background(), tenant, contactID, "psid-1", "Maria Silva")
	if err != nil {
		t.Fatalf("UpdateDisplayName: %v", err)
	}
	if !updated {
		t.Fatal("expected updated=true")
	}

	var got string
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT display_name FROM contact WHERE id = $1`, contactID).Scan(&got); err != nil {
		t.Fatalf("query display_name: %v", err)
	}
	if got != "Maria Silva" {
		t.Errorf("display_name: got %q want %q", got, "Maria Silva")
	}
}

func TestContactBackfillAdapter_UpdateDisplayName_NoopWhenChangedSinceScan(t *testing.T) {
	t.Parallel()
	db := freshDBWithInboxContacts(t)
	store, err := contactbackfill.New(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenant := seedContactsTenant(t, db)
	contactID := seedBackfillContact(t, db, tenant, "psid-1", "messenger", "psid-1")

	// Simulate an operator manually renaming the contact between the
	// scan and this call.
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE contact SET display_name = 'Operator Renamed' WHERE id = $1`, contactID); err != nil {
		t.Fatalf("simulate manual rename: %v", err)
	}

	updated, err := store.UpdateDisplayName(context.Background(), tenant, contactID, "psid-1", "Maria Silva")
	if err != nil {
		t.Fatalf("UpdateDisplayName: %v", err)
	}
	if updated {
		t.Fatal("expected updated=false when the guard no longer matches")
	}

	var got string
	if err := db.AdminPool().QueryRow(context.Background(),
		`SELECT display_name FROM contact WHERE id = $1`, contactID).Scan(&got); err != nil {
		t.Fatalf("query display_name: %v", err)
	}
	if got != "Operator Renamed" {
		t.Errorf("display_name must be untouched: got %q", got)
	}
}
