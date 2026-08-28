package messenger_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
)

// fakeProfileFetcher is the test double for messenger.ProfileFetcher.
type fakeProfileFetcher struct {
	name string
	err  error
}

func (f *fakeProfileFetcher) FetchDisplayName(_ context.Context, _ string) (string, error) {
	return f.name, f.err
}

func newAdapterWithProfile(t *testing.T, in *fakeInbox, r *fakeResolver, f *fakeFlag, c *fakeClock, p messenger.ProfileFetcher) *messenger.Adapter {
	t.Helper()
	cfg := messenger.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		MaxBodyBytes:   1 << 20,
		PastWindow:     24 * time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: time.Second,
	}
	a, err := messenger.New(cfg, in, r, f, messenger.WithClock(c), messenger.WithProfileFetcher(p))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestHandlePost_PopulatesSenderDisplayNameWhenProfileFetcherWired pins
// the fix: Messenger's webhook carries no name, so a wired ProfileFetcher
// must resolve one before the contact is created.
func TestHandlePost_PopulatesSenderDisplayNameWhenProfileFetcherWired(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	f := newFakeFlag(false)
	f.Set(tenant, true)
	a := newAdapterWithProfile(t, in, r, f, newFakeClock(fixedNow), &fakeProfileFetcher{name: "Maria Silva"})

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := validEnvelope(t, testPageID, "mid-001", testPSID, "olá", tsMs)
	rec := doPost(t, a, body, sign(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got := in.Persisted()
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted, got %d", len(got))
	}
	if got[0].SenderDisplayName != "Maria Silva" {
		t.Errorf("SenderDisplayName: got %q want %q", got[0].SenderDisplayName, "Maria Silva")
	}
}

// TestHandlePost_ProfileFetchFailureNeverBlocksDelivery pins the
// soft-fail contract: an erroring fetcher must not fail or drop the
// message — it just leaves SenderDisplayName empty, the pre-fix
// behaviour.
func TestHandlePost_ProfileFetchFailureNeverBlocksDelivery(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	f := newFakeFlag(false)
	f.Set(tenant, true)
	a := newAdapterWithProfile(t, in, r, f, newFakeClock(fixedNow), &fakeProfileFetcher{err: errors.New("graph unavailable")})

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := validEnvelope(t, testPageID, "mid-001", testPSID, "olá", tsMs)
	rec := doPost(t, a, body, sign(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (fetch failure must not fail delivery), got %d", rec.Code)
	}
	got := in.Persisted()
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted, got %d", len(got))
	}
	if got[0].SenderDisplayName != "" {
		t.Errorf("SenderDisplayName: got %q want empty", got[0].SenderDisplayName)
	}
}

// TestHandlePost_NoProfileFetcherLeavesSenderDisplayNameEmpty pins the
// unwired (nil) case — the pre-fix behaviour is unchanged for
// deployments without a configured token.
func TestHandlePost_NoProfileFetcherLeavesSenderDisplayNameEmpty(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	f := newFakeFlag(false)
	f.Set(tenant, true)
	a := newAdapter(t, in, r, f, newFakeClock(fixedNow))

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := validEnvelope(t, testPageID, "mid-001", testPSID, "olá", tsMs)
	rec := doPost(t, a, body, sign(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got := in.Persisted()
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted, got %d", len(got))
	}
	if got[0].SenderDisplayName != "" {
		t.Errorf("SenderDisplayName: got %q want empty", got[0].SenderDisplayName)
	}
}
