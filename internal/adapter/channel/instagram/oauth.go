package instagram

// Business Login for Instagram OAuth mechanics — the authorize redirect
// plus the two-step token exchange described in sender.go's package doc
// comment (authorize -> code exchange -> 60-day long-lived upgrade).
// This is composition-root/admin-flow code (the one-time "connect this
// tenant's Instagram account" action from /settings/channels), not the
// outbound send path, so it maps failures to a local sentinel
// (ErrOAuthExchangeFailed) rather than inbox.ErrChannel*.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthCallbackPath is the single source of truth for the Business Login
// redirect path. Both the "start" redirect builder
// (internal/web/channels) and the composition root's route registration
// (cmd/server/instagram_oauth_wire.go) reference this constant so they
// can never drift from each other or from what's configured in the Meta
// App Dashboard's OAuth redirect-URI allow-list.
const OAuthCallbackPath = "/webhooks/instagram/oauth/callback"

const (
	authorizeBaseURL   = "https://www.instagram.com/oauth/authorize"
	shortLivedTokenURL = "https://api.instagram.com/oauth/access_token"
	longLivedTokenURL  = "https://graph.instagram.com/access_token"
)

// defaultOAuthScope covers everything the outbound Sender actually uses
// (send/receive Instagram Direct messages). Override via
// OAuthConfig.Scope if a future feature needs
// manage_comments/content_publish/insights.
const defaultOAuthScope = "instagram_business_basic,instagram_business_manage_messages"

// ErrOAuthExchangeFailed wraps any non-2xx or malformed response from
// Instagram's token-exchange endpoints.
var ErrOAuthExchangeFailed = errors.New("instagram: oauth token exchange failed")

// ErrSubscribeAppFailed wraps any non-2xx response from the
// subscribed_apps call SubscribeApp makes.
var ErrSubscribeAppFailed = errors.New("instagram: subscribe app failed")

// OAuthConfig holds the Meta App credentials needed to run Business
// Login for Instagram.
type OAuthConfig struct {
	AppID      string
	AppSecret  string
	Scope      string       // defaults to defaultOAuthScope when empty
	HTTPClient *http.Client // defaults to a client with DefaultTimeout when nil

	// ShortLivedTokenURL / LongLivedTokenURL / SubscribedAppsBaseURL
	// override the real Meta endpoints. Tests point these at an
	// httptest.Server; production code leaves them empty to use
	// shortLivedTokenURL/longLivedTokenURL/DefaultBaseURL.
	ShortLivedTokenURL    string
	LongLivedTokenURL     string
	SubscribedAppsBaseURL string
}

func (c OAuthConfig) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c OAuthConfig) scope() string {
	if strings.TrimSpace(c.Scope) != "" {
		return c.Scope
	}
	return defaultOAuthScope
}

func (c OAuthConfig) shortLivedURL() string {
	if c.ShortLivedTokenURL != "" {
		return c.ShortLivedTokenURL
	}
	return shortLivedTokenURL
}

func (c OAuthConfig) longLivedURL() string {
	if c.LongLivedTokenURL != "" {
		return c.LongLivedTokenURL
	}
	return longLivedTokenURL
}

// AuthorizeURL builds the Business Login authorize redirect for an
// already-computed redirectURI and signed state.
func (c OAuthConfig) AuthorizeURL(redirectURI, state string) string {
	v := url.Values{}
	v.Set("client_id", c.AppID)
	v.Set("redirect_uri", redirectURI)
	v.Set("response_type", "code")
	v.Set("scope", c.scope())
	v.Set("state", state)
	return authorizeBaseURL + "?" + v.Encode()
}

// ExchangeCode exchanges the authorization code Instagram redirected
// back with for a short-lived access token. redirectURI MUST match the
// one used in the original AuthorizeURL call byte-for-byte, or Instagram
// rejects the exchange.
func (c OAuthConfig) ExchangeCode(ctx context.Context, redirectURI, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", c.AppID)
	form.Set("client_secret", c.AppSecret)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	form.Set("code", code)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.shortLivedURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("instagram: build code-exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrOAuthExchangeFailed, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("%w: read response: %v", ErrOAuthExchangeFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: status %d: %s", ErrOAuthExchangeFailed, resp.StatusCode, truncateBody(body))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("%w: parse response: %v", ErrOAuthExchangeFailed, err)
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("%w: response missing access_token", ErrOAuthExchangeFailed)
	}
	return parsed.AccessToken, nil
}

// ExchangeLongLivedToken upgrades a short-lived token (valid ~1h) to a
// 60-day long-lived one.
func (c OAuthConfig) ExchangeLongLivedToken(ctx context.Context, shortLivedToken string) (string, time.Duration, error) {
	v := url.Values{}
	v.Set("grant_type", "ig_exchange_token")
	v.Set("client_secret", c.AppSecret)
	v.Set("access_token", shortLivedToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.longLivedURL()+"?"+v.Encode(), nil)
	if err != nil {
		return "", 0, fmt.Errorf("instagram: build long-lived exchange request: %w", err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrOAuthExchangeFailed, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", 0, fmt.Errorf("%w: read response: %v", ErrOAuthExchangeFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("%w: status %d: %s", ErrOAuthExchangeFailed, resp.StatusCode, truncateBody(body))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("%w: parse response: %v", ErrOAuthExchangeFailed, err)
	}
	if parsed.AccessToken == "" || parsed.ExpiresIn <= 0 {
		return "", 0, fmt.Errorf("%w: response missing access_token/expires_in", ErrOAuthExchangeFailed)
	}
	return parsed.AccessToken, time.Duration(parsed.ExpiresIn) * time.Second, nil
}

// SubscribeApp subscribes the tenant's Instagram professional account to
// this app's webhook for the "messages" field
// (POST /{ig-id}/subscribed_apps?subscribed_fields=messages, using the
// account's own access token as bearer auth). Meta requires this
// per-account opt-in independently of the App Dashboard's Webhooks
// product config: without it, a newly-connected account's inbound
// events are never delivered, even with a valid access token and a
// correctly configured app-level webhook.
func (c OAuthConfig) SubscribeApp(ctx context.Context, igBusinessID, accessToken string) error {
	base := c.SubscribedAppsBaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	v := url.Values{}
	v.Set("subscribed_fields", "messages")
	v.Set("access_token", accessToken)
	target := strings.TrimRight(base, "/") + "/" + igBusinessID + "/subscribed_apps?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return fmt.Errorf("instagram: build subscribe-app request: %w", err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSubscribeAppFailed, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrSubscribeAppFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: status %d: %s", ErrSubscribeAppFailed, resp.StatusCode, truncateBody(body))
	}
	return nil
}

func truncateBody(b []byte) string {
	const limit = 256
	if len(b) > limit {
		return string(b[:limit])
	}
	return string(b)
}
