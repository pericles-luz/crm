package instagram_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

// fakeProfileFetcher is the test double for instagram.ProfileFetcher.
type fakeProfileFetcher struct {
	name string
	err  error
}

func (f *fakeProfileFetcher) FetchDisplayName(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	return f.name, f.err
}

func newAdapterWithProfile(t *testing.T, p instagram.ProfileFetcher) (*instagram.Adapter, *deps) {
	t.Helper()
	d := &deps{
		inbox:    newFakeInbox(),
		resolver: newFakeResolver(),
		flag:     newFakeFlag(true),
		rate:     newFakeRateLimiter(0),
		clock:    newFakeClock(time.Unix(1700000000, 0).UTC()),
		media:    newFakeMediaPublisher(),
	}
	cfg := instagram.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  600,
		MaxBodyBytes:   1 << 20,
		PastWindow:     24 * time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: 2 * time.Second,
	}
	a, err := instagram.New(cfg, d.inbox, d.resolver, d.flag, d.rate,
		instagram.WithClock(d.clock),
		instagram.WithMediaScanPublisher(d.media),
		instagram.WithProfileFetcher(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, d
}

// TestHandlePost_PopulatesSenderDisplayNameWhenProfileFetcherWired pins
// the fix: Instagram's webhook carries no name, so a wired ProfileFetcher
// must resolve one before the contact is created.
func TestHandlePost_PopulatesSenderDisplayNameWhenProfileFetcherWired(t *testing.T) {
	t.Parallel()
	a, d := newAdapterWithProfile(t, &fakeProfileFetcher{name: "Maria Silva"})
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	now := d.clock.Now()
	ts := now.UnixMilli()
	body := buildEnvelope("igb-1", ts, msgInbound("igsid-1", "mid-1", "hello", ts, nil))

	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	got := d.inbox.Persisted()
	if len(got) != 1 {
		t.Fatalf("persisted: got %d, want 1", len(got))
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
	a, d := newAdapterWithProfile(t, &fakeProfileFetcher{err: errors.New("graph unavailable")})
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	now := d.clock.Now()
	ts := now.UnixMilli()
	body := buildEnvelope("igb-1", ts, msgInbound("igsid-1", "mid-1", "hello", ts, nil))

	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d (fetch failure must not fail delivery), want 200", resp.Code)
	}
	got := d.inbox.Persisted()
	if len(got) != 1 {
		t.Fatalf("persisted: got %d, want 1", len(got))
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
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	now := d.clock.Now()
	ts := now.UnixMilli()
	body := buildEnvelope("igb-1", ts, msgInbound("igsid-1", "mid-1", "hello", ts, nil))

	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	got := d.inbox.Persisted()
	if len(got) != 1 {
		t.Fatalf("persisted: got %d, want 1", len(got))
	}
	if got[0].SenderDisplayName != "" {
		t.Errorf("SenderDisplayName: got %q want empty", got[0].SenderDisplayName)
	}
}
