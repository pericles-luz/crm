package channels_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
	webchannels "github.com/pericles-luz/crm/internal/web/channels"
)

// fakeInstagramConnector is a controllable webchannels.InstagramConnector
// for the "Conectar Instagram" flow.
type fakeInstagramConnector struct {
	authorizeURL   string
	authorizeErr   error
	connected      bool
	expiresAt      time.Time
	statusErr      error
	gotTenantID    uuid.UUID
	gotRedirectURI string
}

func (f *fakeInstagramConnector) AuthorizeURL(tenantID uuid.UUID, redirectURI string) (string, error) {
	f.gotTenantID = tenantID
	f.gotRedirectURI = redirectURI
	if f.authorizeErr != nil {
		return "", f.authorizeErr
	}
	return f.authorizeURL, nil
}

func (f *fakeInstagramConnector) Status(_ context.Context, _ uuid.UUID) (bool, time.Time, error) {
	return f.connected, f.expiresAt, f.statusErr
}

func newHandlerInstagramOAuth(t *testing.T, repo *fakeRepo, acc *fakeAccess, oauth webchannels.InstagramConnector) http.Handler {
	t.Helper()
	h, err := webchannels.New(webchannels.Deps{Channels: repo, Access: acc, InstagramOAuth: oauth})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func TestConnectInstagram_RedirectsToAuthorizeURL(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	ch := mkChannel(t, repo, "instagram", "123456789012345", "Conta do Instagram", true)
	oauth := &fakeInstagramConnector{authorizeURL: "https://www.instagram.com/oauth/authorize?client_id=abc"}
	mux := newHandlerInstagramOAuth(t, repo, acc, oauth)

	req := httptest.NewRequest(http.MethodGet, "/settings/channels/"+ch.ID.String()+"/connect-instagram", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req = req.WithContext(tenancy.WithContext(req.Context(), testTenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302, body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != oauth.authorizeURL {
		t.Fatalf("Location = %q, want %q", loc, oauth.authorizeURL)
	}
	if oauth.gotTenantID != testTenant.ID {
		t.Errorf("AuthorizeURL called with tenant %s, want %s", oauth.gotTenantID, testTenant.ID)
	}
	if !strings.Contains(oauth.gotRedirectURI, "/webhooks/instagram/oauth/callback") {
		t.Errorf("redirectURI = %q, want the oauth callback path", oauth.gotRedirectURI)
	}
	if !strings.HasPrefix(oauth.gotRedirectURI, "https://") {
		t.Errorf("redirectURI = %q, want https scheme (X-Forwarded-Proto honored)", oauth.gotRedirectURI)
	}
}

func TestConnectInstagram_NotFoundWhenOAuthNotConfigured(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	ch := mkChannel(t, repo, "instagram", "123456789012345", "Conta do Instagram", true)
	mux := newHandler(t, repo, acc) // no InstagramOAuth wired

	req := httptest.NewRequest(http.MethodGet, "/settings/channels/"+ch.ID.String()+"/connect-instagram", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), testTenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestConnectInstagram_NotFoundForNonInstagramChannel(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	ch := mkChannel(t, repo, "whatsapp", "5511999990000", "WhatsApp", true)
	oauth := &fakeInstagramConnector{authorizeURL: "https://example.com/authorize"}
	mux := newHandlerInstagramOAuth(t, repo, acc, oauth)

	req := httptest.NewRequest(http.MethodGet, "/settings/channels/"+ch.ID.String()+"/connect-instagram", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), testTenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestChannelsPage_ShowsConnectInstagramButton(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	mkChannel(t, repo, "instagram", "123456789012345", "Conta do Instagram", true)
	oauth := &fakeInstagramConnector{}
	mux := newHandlerInstagramOAuth(t, repo, acc, oauth)

	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "connect-instagram") {
		t.Fatalf("expected connect-instagram link in body:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Conectado") {
		t.Fatalf("expected NOT connected state (no stored token), body:\n%s", rec.Body.String())
	}
}

func TestChannelsPage_ShowsConnectedBadge(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	mkChannel(t, repo, "instagram", "123456789012345", "Conta do Instagram", true)
	oauth := &fakeInstagramConnector{connected: true}
	mux := newHandlerInstagramOAuth(t, repo, acc, oauth)

	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Conectado") {
		t.Fatalf("expected Conectado badge, body:\n%s", rec.Body.String())
	}
}

func TestChannelsPage_HidesConnectButtonWhenOAuthNotConfigured(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	mkChannel(t, repo, "instagram", "123456789012345", "Conta do Instagram", true)
	mux := newHandler(t, repo, acc) // no InstagramOAuth wired

	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "connect-instagram") {
		t.Fatalf("expected no connect-instagram link when OAuth isn't configured, body:\n%s", rec.Body.String())
	}
}

func TestChannelsPage_InstagramConnectedToast(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	mux := newHandler(t, repo, acc)

	rec := do(t, mux, http.MethodGet, "/settings/channels?instagram=connected", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Instagram conectado com sucesso.") {
		t.Fatalf("expected success toast, body:\n%s", rec.Body.String())
	}
}

func TestChannelsPage_InstagramErrorToast(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	mux := newHandler(t, repo, acc)

	rec := do(t, mux, http.MethodGet, "/settings/channels?instagram=error", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Não foi possível conectar o Instagram") {
		t.Fatalf("expected error toast, body:\n%s", rec.Body.String())
	}
}
