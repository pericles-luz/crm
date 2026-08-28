package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/contactbackfill"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoadConfig_RequiresMasterOpsDSN(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(nil, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected error when MASTER_OPS_DATABASE_URL is unset")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == "MASTER_OPS_DATABASE_URL" {
			return "postgres://x"
		}
		return ""
	}
	cfg, err := loadConfig(nil, getenv)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.apply {
		t.Error("apply should default to false (dry-run)")
	}
	if cfg.tenant != uuid.Nil {
		t.Error("tenant should default to uuid.Nil (all tenants)")
	}
	if cfg.channel != "" {
		t.Error("channel should default to empty (both)")
	}
	if cfg.limit != 0 {
		t.Error("limit should default to 0 (no limit)")
	}
	if cfg.delay != 300*time.Millisecond {
		t.Errorf("delay: got %v want 300ms", cfg.delay)
	}
}

func TestLoadConfig_ParsesFlags(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == "MASTER_OPS_DATABASE_URL" {
			return "postgres://x"
		}
		return ""
	}
	tenantID := uuid.New()
	cfg, err := loadConfig([]string{
		"-apply", "-tenant", tenantID.String(), "-channel", "instagram", "-limit", "5", "-delay", "50ms",
	}, getenv)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.apply {
		t.Error("apply should be true")
	}
	if cfg.tenant != tenantID {
		t.Errorf("tenant: got %s want %s", cfg.tenant, tenantID)
	}
	if cfg.channel != "instagram" {
		t.Errorf("channel: got %q want instagram", cfg.channel)
	}
	if cfg.limit != 5 {
		t.Errorf("limit: got %d want 5", cfg.limit)
	}
	if cfg.delay != 50*time.Millisecond {
		t.Errorf("delay: got %v want 50ms", cfg.delay)
	}
}

func TestLoadConfig_RejectsInvalidTenant(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == "MASTER_OPS_DATABASE_URL" {
			return "postgres://x"
		}
		return ""
	}
	if _, err := loadConfig([]string{"-tenant", "not-a-uuid"}, getenv); err == nil {
		t.Fatal("expected error for invalid -tenant")
	}
}

func TestLoadConfig_RejectsInvalidChannel(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == "MASTER_OPS_DATABASE_URL" {
			return "postgres://x"
		}
		return ""
	}
	if _, err := loadConfig([]string{"-channel", "whatsapp"}, getenv); err == nil {
		t.Fatal("expected error for invalid -channel")
	}
}

func TestMessengerGraphToken_Precedence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		specific, gen string
		want          string
	}{
		{"specific wins", "specific-tok", "generic-tok", "specific-tok"},
		{"falls back to generic", "", "generic-tok", "generic-tok"},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string {
				switch k {
				case "META_MESSENGER_GRAPH_TOKEN":
					return tc.specific
				case "META_GRAPH_TOKEN":
					return tc.gen
				}
				return ""
			}
			if got := messengerGraphToken(getenv); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// --- runWith fakes ---------------------------------------------------------

type fakeStore struct {
	candidates []contactbackfill.Candidate
	listErr    error

	updateCalls []updateCall
	updateFn    func(tenantID, contactID uuid.UUID, oldExternalID, newName string) (bool, error)
}

type updateCall struct {
	tenantID, contactID    uuid.UUID
	oldExternalID, newName string
}

func (f *fakeStore) ListCandidates(_ context.Context, _ string) ([]contactbackfill.Candidate, error) {
	return f.candidates, f.listErr
}

func (f *fakeStore) UpdateDisplayName(_ context.Context, tenantID, contactID uuid.UUID, oldExternalID, newName string) (bool, error) {
	f.updateCalls = append(f.updateCalls, updateCall{tenantID, contactID, oldExternalID, newName})
	if f.updateFn != nil {
		return f.updateFn(tenantID, contactID, oldExternalID, newName)
	}
	return true, nil
}

type fakeMessengerFetcher struct {
	name string
	err  error
}

func (f *fakeMessengerFetcher) FetchDisplayName(context.Context, string) (string, error) {
	return f.name, f.err
}

type fakeInstagramFetcher struct {
	name string
	err  error
}

func (f *fakeInstagramFetcher) FetchDisplayName(context.Context, uuid.UUID, string) (string, error) {
	return f.name, f.err
}

// --- runWith tests -----------------------------------------------------

func TestRunWith_DryRunAppliesNothing(t *testing.T) {
	t.Parallel()
	tenantID, contactID := uuid.New(), uuid.New()
	store := &fakeStore{candidates: []contactbackfill.Candidate{
		{ContactID: contactID, TenantID: tenantID, Channel: "messenger", ExternalID: "psid-1"},
	}}
	sum, err := runWith(context.Background(), discardLogger(), config{delay: 0}, store, &fakeMessengerFetcher{name: "Maria"}, &fakeInstagramFetcher{})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if sum.WouldApply != 1 || sum.Applied != 0 {
		t.Errorf("summary: got %+v", sum)
	}
	if len(store.updateCalls) != 0 {
		t.Errorf("dry-run must not call UpdateDisplayName, got %d calls", len(store.updateCalls))
	}
}

func TestRunWith_ApplyWritesResolvedName(t *testing.T) {
	t.Parallel()
	tenantID, contactID := uuid.New(), uuid.New()
	store := &fakeStore{candidates: []contactbackfill.Candidate{
		{ContactID: contactID, TenantID: tenantID, Channel: "instagram", ExternalID: "igsid-1"},
	}}
	sum, err := runWith(context.Background(), discardLogger(), config{apply: true, delay: 0}, store, &fakeMessengerFetcher{}, &fakeInstagramFetcher{name: "Maria Silva"})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if sum.Applied != 1 {
		t.Errorf("summary: got %+v", sum)
	}
	if len(store.updateCalls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(store.updateCalls))
	}
	got := store.updateCalls[0]
	if got.newName != "Maria Silva" || got.oldExternalID != "igsid-1" || got.tenantID != tenantID || got.contactID != contactID {
		t.Errorf("update call: got %+v", got)
	}
}

func TestRunWith_SkipsWhenChangedSinceScan(t *testing.T) {
	t.Parallel()
	store := &fakeStore{
		candidates: []contactbackfill.Candidate{
			{ContactID: uuid.New(), TenantID: uuid.New(), Channel: "messenger", ExternalID: "psid-1"},
		},
		updateFn: func(uuid.UUID, uuid.UUID, string, string) (bool, error) { return false, nil },
	}
	sum, err := runWith(context.Background(), discardLogger(), config{apply: true, delay: 0}, store, &fakeMessengerFetcher{name: "Maria"}, &fakeInstagramFetcher{})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if sum.SkippedChanged != 1 || sum.Applied != 0 {
		t.Errorf("summary: got %+v", sum)
	}
}

func TestRunWith_FetchErrorSkipsWithoutAborting(t *testing.T) {
	t.Parallel()
	store := &fakeStore{candidates: []contactbackfill.Candidate{
		{ContactID: uuid.New(), TenantID: uuid.New(), Channel: "messenger", ExternalID: "psid-1"},
		{ContactID: uuid.New(), TenantID: uuid.New(), Channel: "instagram", ExternalID: "igsid-1"},
	}}
	sum, err := runWith(context.Background(), discardLogger(), config{apply: true, delay: 0}, store,
		&fakeMessengerFetcher{err: errors.New("graph unavailable")},
		&fakeInstagramFetcher{name: "Maria"})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if sum.SkippedNoName != 1 {
		t.Errorf("expected 1 skipped-no-name, got %+v", sum)
	}
	if sum.Applied != 1 {
		t.Errorf("expected the second (instagram) candidate to still apply, got %+v", sum)
	}
}

func TestRunWith_NoFetcherWiredSkipsChannel(t *testing.T) {
	t.Parallel()
	store := &fakeStore{candidates: []contactbackfill.Candidate{
		{ContactID: uuid.New(), TenantID: uuid.New(), Channel: "messenger", ExternalID: "psid-1"},
	}}
	sum, err := runWith(context.Background(), discardLogger(), config{apply: true, delay: 0}, store, nil, &fakeInstagramFetcher{})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if sum.SkippedNoFetcher != 1 {
		t.Errorf("summary: got %+v", sum)
	}
}

func TestRunWith_TenantFilter(t *testing.T) {
	t.Parallel()
	wantTenant := uuid.New()
	otherTenant := uuid.New()
	store := &fakeStore{candidates: []contactbackfill.Candidate{
		{ContactID: uuid.New(), TenantID: wantTenant, Channel: "messenger", ExternalID: "psid-1"},
		{ContactID: uuid.New(), TenantID: otherTenant, Channel: "messenger", ExternalID: "psid-2"},
	}}
	sum, err := runWith(context.Background(), discardLogger(), config{tenant: wantTenant, delay: 0}, store, &fakeMessengerFetcher{name: "Maria"}, &fakeInstagramFetcher{})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if sum.Scanned != 1 {
		t.Errorf("tenant filter: scanned=%d want 1", sum.Scanned)
	}
}

func TestRunWith_LimitStopsEarly(t *testing.T) {
	t.Parallel()
	store := &fakeStore{candidates: []contactbackfill.Candidate{
		{ContactID: uuid.New(), TenantID: uuid.New(), Channel: "messenger", ExternalID: "psid-1"},
		{ContactID: uuid.New(), TenantID: uuid.New(), Channel: "messenger", ExternalID: "psid-2"},
		{ContactID: uuid.New(), TenantID: uuid.New(), Channel: "messenger", ExternalID: "psid-3"},
	}}
	sum, err := runWith(context.Background(), discardLogger(), config{limit: 1, delay: 0}, store, &fakeMessengerFetcher{name: "Maria"}, &fakeInstagramFetcher{})
	if err != nil {
		t.Fatalf("runWith: %v", err)
	}
	if sum.Scanned != 1 {
		t.Errorf("limit: scanned=%d want 1", sum.Scanned)
	}
}

func TestRunWith_ListCandidatesErrorPropagates(t *testing.T) {
	t.Parallel()
	store := &fakeStore{listErr: errors.New("boom")}
	_, err := runWith(context.Background(), discardLogger(), config{delay: 0}, store, &fakeMessengerFetcher{}, &fakeInstagramFetcher{})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}
