package instagram_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/channel/instagram"
)

func TestOAuthConfig_AuthorizeURL(t *testing.T) {
	t.Parallel()
	cfg := instagram.OAuthConfig{AppID: "app123", AppSecret: "shh"}
	got := cfg.AuthorizeURL("https://acme.example.com/webhooks/instagram/oauth/callback", "the-state")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("AuthorizeURL produced invalid URL: %v", err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://www.instagram.com/oauth/authorize" {
		t.Fatalf("unexpected base: %s", got)
	}
	q := u.Query()
	if q.Get("client_id") != "app123" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "https://acme.example.com/webhooks/instagram/oauth/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("state") != "the-state" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("scope") != "instagram_business_basic,instagram_business_manage_messages" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
}

func TestOAuthConfig_AuthorizeURL_CustomScope(t *testing.T) {
	t.Parallel()
	cfg := instagram.OAuthConfig{AppID: "app123", AppSecret: "shh", Scope: "instagram_business_basic"}
	u, _ := url.Parse(cfg.AuthorizeURL("https://x/callback", "s"))
	if got := u.Query().Get("scope"); got != "instagram_business_basic" {
		t.Errorf("scope = %q", got)
	}
}

func TestOAuthConfig_ExchangeCode_Success(t *testing.T) {
	t.Parallel()
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"short-lived-abc","user_id":123}`))
	}))
	defer srv.Close()

	cfg := instagram.OAuthConfig{AppID: "app123", AppSecret: "shh", HTTPClient: srv.Client(), ShortLivedTokenURL: srv.URL}

	tok, err := cfg.ExchangeCode(context.Background(), "https://acme.example.com/cb", "the-code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok != "short-lived-abc" {
		t.Errorf("token = %q", tok)
	}
	if gotForm.Get("code") != "the-code" {
		t.Errorf("code = %q", gotForm.Get("code"))
	}
	if gotForm.Get("client_secret") != "shh" {
		t.Errorf("client_secret = %q", gotForm.Get("client_secret"))
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", gotForm.Get("grant_type"))
	}
}

func TestOAuthConfig_ExchangeCode_NonOKStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_message":"invalid code"}`))
	}))
	defer srv.Close()

	cfg := instagram.OAuthConfig{AppID: "a", AppSecret: "s", HTTPClient: srv.Client(), ShortLivedTokenURL: srv.URL}
	_, err := cfg.ExchangeCode(context.Background(), "https://x/cb", "bad-code")
	if err == nil || !strings.Contains(err.Error(), "invalid code") {
		t.Fatalf("expected wrapped error containing response body, got %v", err)
	}
}

func TestOAuthConfig_ExchangeLongLivedToken_Success(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("access_token"); got != "short-lived-abc" {
			t.Errorf("access_token query param = %q", got)
		}
		if got := r.URL.Query().Get("grant_type"); got != "ig_exchange_token" {
			t.Errorf("grant_type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"long-lived-xyz","expires_in":5184000}`))
	}))
	defer srv.Close()

	cfg := instagram.OAuthConfig{AppID: "a", AppSecret: "s", HTTPClient: srv.Client(), LongLivedTokenURL: srv.URL}
	tok, ttl, err := cfg.ExchangeLongLivedToken(context.Background(), "short-lived-abc")
	if err != nil {
		t.Fatalf("ExchangeLongLivedToken: %v", err)
	}
	if tok != "long-lived-xyz" {
		t.Errorf("token = %q", tok)
	}
	if ttl != 5184000*time.Second {
		t.Errorf("ttl = %v", ttl)
	}
}

func TestOAuthConfig_ExchangeLongLivedToken_MissingFields(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := instagram.OAuthConfig{AppID: "a", AppSecret: "s", HTTPClient: srv.Client(), LongLivedTokenURL: srv.URL}
	if _, _, err := cfg.ExchangeLongLivedToken(context.Background(), "tok"); err == nil {
		t.Fatal("expected error for missing access_token/expires_in")
	}
}
