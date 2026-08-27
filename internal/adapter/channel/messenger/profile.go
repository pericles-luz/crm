package messenger

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

// ProfileFetchTimeout caps the whole profile lookup. It is deliberately
// short and single-attempt (unlike Sender's retry/backoff machinery): a
// slow or failing name lookup must never delay or block delivery of the
// customer's message.
const ProfileFetchTimeout = 3 * time.Second

// ErrProfileFetchFailed wraps any non-2xx Graph response or transport
// error from FetchDisplayName.
var ErrProfileFetchFailed = errors.New("messenger: profile fetch failed")

// ProfileFetcher resolves a PSID's display name via the Meta Graph API
// User node (GET /{psid}?fields=first_name,last_name). Messenger's
// messaging[] webhook payload carries only the sender's scoped id — no
// name — unlike WhatsApp's webhook, which includes contacts[].profile.name
// directly. This is the follow-up call that closes that gap.
//
// Reuses the same token/host family as Sender (SecretScope: AppLevel —
// one system-user token signs all page calls, so no per-tenant lookup is
// needed here, unlike Instagram's per-tenant OAuth token).
type ProfileFetcher struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// ProfileOption configures a ProfileFetcher.
type ProfileOption func(*ProfileFetcher)

// WithProfileHTTPClient overrides the default http.Client. Tests use
// this to point at an httptest.Server with a short timeout.
func WithProfileHTTPClient(c *http.Client) ProfileOption {
	return func(p *ProfileFetcher) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// WithProfileBaseURL overrides the Meta Graph base URL.
func WithProfileBaseURL(url string) ProfileOption {
	return func(p *ProfileFetcher) {
		if url != "" {
			p.baseURL = url
		}
	}
}

// NewProfileFetcher constructs a ProfileFetcher. token must be non-empty.
func NewProfileFetcher(token string, opts ...ProfileOption) (*ProfileFetcher, error) {
	if token == "" {
		return nil, errors.New("messenger: profile fetcher token must not be empty")
	}
	p := &ProfileFetcher{
		httpClient: &http.Client{Timeout: ProfileFetchTimeout},
		baseURL:    DefaultBaseURL,
		token:      token,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// profileResponse is the subset of the Graph User node this adapter
// reads. Messenger's PSID profile node has only allowed first_name /
// last_name / profile_pic since the 2018 Platform policy change — there
// is no combined "name" field to fall back to.
type profileResponse struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// FetchDisplayName resolves psid's display name. Returns
// ErrProfileFetchFailed (wrapped) on any transport error or non-2xx
// response, and an error when both name fields come back empty — the
// caller treats any error as "no name available" and proceeds with the
// existing fallback, never failing the inbound delivery on this account.
func (p *ProfileFetcher) FetchDisplayName(ctx context.Context, psid string) (string, error) {
	psid = strings.TrimSpace(psid)
	if psid == "" {
		return "", errors.New("messenger: psid must not be empty")
	}
	v := url.Values{}
	v.Set("fields", "first_name,last_name")
	v.Set("access_token", p.token)
	target := strings.TrimRight(p.baseURL, "/") + "/" + psid + "?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build request: %v", ErrProfileFetchFailed, err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrProfileFetchFailed, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("%w: read response: %v", ErrProfileFetchFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: status %d: %s", ErrProfileFetchFailed, resp.StatusCode, string(body))
	}
	var out profileResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("%w: decode response: %v", ErrProfileFetchFailed, err)
	}
	name := strings.TrimSpace(strings.TrimSpace(out.FirstName) + " " + strings.TrimSpace(out.LastName))
	if name == "" {
		return "", fmt.Errorf("%w: empty name in response", ErrProfileFetchFailed)
	}
	return name, nil
}
