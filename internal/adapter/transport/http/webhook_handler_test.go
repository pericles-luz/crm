package http_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metaadapter "github.com/pericles-luz/crm/internal/adapter/channel/meta"
	httpadapter "github.com/pericles-luz/crm/internal/adapter/transport/http"
	"github.com/pericles-luz/crm/internal/webhook"
)

const testSecret = "abcd"

type stubTokenStore struct {
	tenant webhook.TenantID
	err    error
}

func (s *stubTokenStore) Lookup(context.Context, string, []byte, time.Time) (webhook.TenantID, error) {
	return s.tenant, s.err
}
func (s *stubTokenStore) MarkUsed(context.Context, string, []byte, time.Time) error { return nil }

type stubIdem struct {
	mu   sync.Mutex
	seen map[[32]byte]bool
}

func newIdem() *stubIdem { return &stubIdem{seen: map[[32]byte]bool{}} }
func (s *stubIdem) CheckAndStore(_ context.Context, _ webhook.TenantID, _ string, key []byte, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k [32]byte
	copy(k[:], key)
	if s.seen[k] {
		return false, nil
	}
	s.seen[k] = true
	return true, nil
}

type stubRaw struct {
	mu   sync.Mutex
	rows []webhook.RawEventRow
	id   [16]byte
}

func (s *stubRaw) Insert(_ context.Context, row webhook.RawEventRow) ([16]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
	s.id[15]++
	return s.id, nil
}
func (s *stubRaw) MarkPublished(context.Context, [16]byte, time.Time) error { return nil }

type stubPub struct{ calls int }

func (p *stubPub) Publish(context.Context, [16]byte, webhook.TenantID, string, []byte, map[string][]string) error {
	p.calls++
	return nil
}

// stubAssoc allow-by-default; the Meta adapter only emits an
// association when the body has phone_number_id, so existing
// happy-path tests using payloads without that field skip the check.
type stubAssoc struct{}

func (stubAssoc) CheckAssociation(context.Context, webhook.TenantID, string, string) (bool, error) {
	return true, nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newServer(t *testing.T) (*httptest.Server, *stubRaw, *stubPub, *stubIdem, *stubTokenStore) {
	t.Helper()
	adapter, err := metaadapter.New("whatsapp", testSecret)
	if err != nil {
		t.Fatalf("meta New: %v", err)
	}
	idem := newIdem()
	raw := &stubRaw{}
	pub := &stubPub{}
	tokens := &stubTokenStore{tenant: webhook.TenantID{0xaa}}
	svc, err := webhook.NewService(webhook.Config{
		Adapters:               []webhook.ChannelAdapter{adapter},
		TokenStore:             tokens,
		IdempotencyStore:       idem,
		RawEventStore:          raw,
		Publisher:              pub,
		TenantAssociationStore: stubAssoc{},
		Clock:                  fixedClock{t: time.Unix(1_700_000_000, 0).UTC()},
		AsyncRunner:            func(f func()) { f() },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mux := http.NewServeMux()
	httpadapter.NewHandler(svc).Register(mux)
	return httptest.NewServer(mux), raw, pub, idem, tokens
}

func TestHandler_Returns200WithJSONOnAccepted(t *testing.T) {
	t.Parallel()
	srv, raw, pub, _, _ := newServer(t)
	defer srv.Close()

	body := []byte(`{"entry":[{"id":"1","time":1700000000}]}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/whatsapp/sometoken", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Hub-Signature-256", sign(body))
	req.Header.Set("X-Request-Id", "req-1")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	bs, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(bs), "ok") {
		t.Fatalf("body = %q", bs)
	}
	if len(raw.rows) != 1 || pub.calls != 1 {
		t.Fatalf("raw=%d pub=%d, want 1 each", len(raw.rows), pub.calls)
	}
}

func TestHandler_AlwaysReturns200OnFailures(t *testing.T) {
	t.Parallel()
	srv, raw, pub, _, _ := newServer(t)
	defer srv.Close()

	body := []byte(`{"entry":[{"id":"1","time":1700000000}]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/whatsapp/sometoken", strings.NewReader(string(body)))
	// no signature header → invalid
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (anti-enumeration)", res.StatusCode)
	}
	if len(raw.rows) != 0 || pub.calls != 0 {
		t.Fatal("invalid signature must not insert/publish")
	}
}

func TestHandler_BodyBitExactness(t *testing.T) {
	t.Parallel()
	srv, raw, _, _, _ := newServer(t)
	defer srv.Close()

	body := []byte("{ \"entry\" : [ { \"id\" : \"x\" , \"time\" : 1700000000 } ] }")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/whatsapp/sometoken", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sign(body))

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if len(raw.rows) != 1 {
		t.Fatalf("raw rows = %d, want 1", len(raw.rows))
	}
	if string(raw.rows[0].Payload) != string(body) {
		t.Fatalf("stored payload != sent payload (whitespace clobbered)")
	}
}

func TestHandler_RoutePathPattern(t *testing.T) {
	t.Parallel()
	srv, _, _, _, _ := newServer(t)
	defer srv.Close()
	res, err := http.Post(srv.URL+"/webhooks/telegram/anytoken", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// Unknown channel still answers 200 (anti-enumeration).
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}
