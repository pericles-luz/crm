package instagram_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channel/instagram"
)

type fakeTokenSaver struct {
	mu    sync.Mutex
	saves []savedToken
	err   error
}

type savedToken struct {
	tenantID    uuid.UUID
	accessToken string
	tokenType   string
	expiresAt   time.Time
}

func (f *fakeTokenSaver) Save(_ context.Context, tenantID uuid.UUID, accessToken, tokenType string, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.saves = append(f.saves, savedToken{tenantID, accessToken, tokenType, expiresAt})
	return nil
}

func exchangeTestServer(t *testing.T) (shortURL, longURL string, close func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"short-lived-tok"}`))
	})
	mux.HandleFunc("/long", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"long-lived-tok","expires_in":5184000}`))
	})
	srv := httptest.NewServer(mux)
	return srv.URL + "/short", srv.URL + "/long", srv.Close
}

func TestOAuthCallbackHandler_Success(t *testing.T) {
	t.Parallel()
	shortURL, longURL, closeSrv := exchangeTestServer(t)
	defer closeSrv()

	secret := []byte("test-secret-at-least-16-bytes")
	tenantID := uuid.New()
	state, err := instagram.SignOAuthState(secret, tenantID, 10*time.Minute)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}

	store := &fakeTokenSaver{}
	cfg := instagram.OAuthConfig{AppID: "app", AppSecret: "secret", ShortLivedTokenURL: shortURL, LongLivedTokenURL: longURL}
	h := instagram.NewOAuthCallbackHandler(cfg, secret, store)

	req := httptest.NewRequest(http.MethodGet, "https://acme.example.com/webhooks/instagram/oauth/callback?code=the-code&state="+state, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/settings/channels?instagram=connected") {
		t.Fatalf("Location = %q, want redirect to settings/channels?instagram=connected", loc)
	}
	if !strings.HasPrefix(loc, "https://acme.example.com") {
		t.Fatalf("Location = %q, want same-host redirect", loc)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saves) != 1 {
		t.Fatalf("want 1 save, got %d", len(store.saves))
	}
	saved := store.saves[0]
	if saved.tenantID != tenantID {
		t.Errorf("tenantID = %s, want %s", saved.tenantID, tenantID)
	}
	if saved.accessToken != "long-lived-tok" {
		t.Errorf("accessToken = %q", saved.accessToken)
	}
	if saved.tokenType != "bearer" {
		t.Errorf("tokenType = %q", saved.tokenType)
	}
	if saved.expiresAt.Before(time.Now().Add(59 * 24 * time.Hour)) {
		t.Errorf("expiresAt too soon: %v", saved.expiresAt)
	}
}

func TestOAuthCallbackHandler_UserDenied(t *testing.T) {
	t.Parallel()
	store := &fakeTokenSaver{}
	h := instagram.NewOAuthCallbackHandler(instagram.OAuthConfig{}, []byte("secret-at-least-16-bytes"), store)

	req := httptest.NewRequest(http.MethodGet, "https://acme.example.com/webhooks/instagram/oauth/callback?error=access_denied&error_description=user+cancelled", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "instagram=error") {
		t.Fatalf("Location = %q, want instagram=error", loc)
	}
	if len(store.saves) != 0 {
		t.Fatalf("expected no save on user-denied path, got %d", len(store.saves))
	}
}

func TestOAuthCallbackHandler_InvalidState(t *testing.T) {
	t.Parallel()
	store := &fakeTokenSaver{}
	h := instagram.NewOAuthCallbackHandler(instagram.OAuthConfig{}, []byte("secret-at-least-16-bytes"), store)

	req := httptest.NewRequest(http.MethodGet, "https://acme.example.com/webhooks/instagram/oauth/callback?code=c&state=garbage", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "instagram=error") {
		t.Fatalf("Location = %q, want instagram=error", loc)
	}
	if len(store.saves) != 0 {
		t.Fatalf("expected no save on invalid state, got %d", len(store.saves))
	}
}

func TestOAuthCallbackHandler_MissingCode(t *testing.T) {
	t.Parallel()
	store := &fakeTokenSaver{}
	secret := []byte("test-secret-at-least-16-bytes")
	state, _ := instagram.SignOAuthState(secret, uuid.New(), 10*time.Minute)
	h := instagram.NewOAuthCallbackHandler(instagram.OAuthConfig{}, secret, store)

	req := httptest.NewRequest(http.MethodGet, "https://acme.example.com/webhooks/instagram/oauth/callback?state="+state, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "instagram=error") {
		t.Fatalf("Location = %q, want instagram=error", loc)
	}
}

func TestOAuthCallbackHandler_ExchangeFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_message":"invalid code"}`))
	}))
	defer srv.Close()

	secret := []byte("test-secret-at-least-16-bytes")
	state, _ := instagram.SignOAuthState(secret, uuid.New(), 10*time.Minute)
	store := &fakeTokenSaver{}
	cfg := instagram.OAuthConfig{AppID: "app", AppSecret: "s", ShortLivedTokenURL: srv.URL}
	h := instagram.NewOAuthCallbackHandler(cfg, secret, store)

	req := httptest.NewRequest(http.MethodGet, "https://acme.example.com/webhooks/instagram/oauth/callback?code=c&state="+state, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "instagram=error") {
		t.Fatalf("Location = %q, want instagram=error", loc)
	}
	if len(store.saves) != 0 {
		t.Fatalf("expected no save on exchange failure, got %d", len(store.saves))
	}
}

func TestOAuthCallbackHandler_SaveFailure(t *testing.T) {
	t.Parallel()
	shortURL, longURL, closeSrv := exchangeTestServer(t)
	defer closeSrv()

	secret := []byte("test-secret-at-least-16-bytes")
	state, _ := instagram.SignOAuthState(secret, uuid.New(), 10*time.Minute)
	store := &fakeTokenSaver{err: context.DeadlineExceeded}
	cfg := instagram.OAuthConfig{AppID: "app", AppSecret: "s", ShortLivedTokenURL: shortURL, LongLivedTokenURL: longURL}
	h := instagram.NewOAuthCallbackHandler(cfg, secret, store)

	req := httptest.NewRequest(http.MethodGet, "https://acme.example.com/webhooks/instagram/oauth/callback?code=c&state="+state, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "instagram=error") {
		t.Fatalf("Location = %q, want instagram=error", loc)
	}
}
