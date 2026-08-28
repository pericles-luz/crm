package instagram

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

	"github.com/google/uuid"
)

// ProfileFetchTimeout caps the whole profile lookup. Deliberately short
// and single-attempt (unlike Sender's retry/backoff machinery): a slow
// or failing name lookup must never delay or block delivery of the
// customer's message.
const ProfileFetchTimeout = 3 * time.Second

// ErrProfileFetchFailed wraps any non-2xx Graph response, transport
// error, or missing token from FetchDisplayName.
var ErrProfileFetchFailed = errors.New("instagram: profile fetch failed")

// TokenLookup resolves the access token to use for a tenant's profile
// lookups. The composition root wires this to the same per-tenant OAuth
// token (with global fallback) the outbound Sender resolves — see
// cmd/server/instagram_outbound_wire.go's TenantConfigLookup for the
// identical precedence.
type TokenLookup func(ctx context.Context, tenantID uuid.UUID) (string, error)

// ProfileFetcher resolves an IGSID's display name via the Instagram
// Graph API user node (GET /{igsid}?fields=name,username). Instagram's
// messaging[] webhook payload carries only the sender's scoped id — no
// name — so this is the follow-up call that closes that gap.
type ProfileFetcher struct {
	httpClient  *http.Client
	baseURL     string
	tokenLookup TokenLookup
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

// WithProfileBaseURL overrides the Instagram Graph base URL.
func WithProfileBaseURL(url string) ProfileOption {
	return func(p *ProfileFetcher) {
		if url != "" {
			p.baseURL = url
		}
	}
}

// NewProfileFetcher constructs a ProfileFetcher. lookup must be non-nil.
func NewProfileFetcher(lookup TokenLookup, opts ...ProfileOption) (*ProfileFetcher, error) {
	if lookup == nil {
		return nil, errors.New("instagram: profile fetcher token lookup must not be nil")
	}
	p := &ProfileFetcher{
		httpClient:  &http.Client{Timeout: ProfileFetchTimeout},
		baseURL:     DefaultBaseURL,
		tokenLookup: lookup,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// profileResponse is the subset of the Instagram Graph user node this
// adapter reads.
type profileResponse struct {
	Name     string `json:"name"`
	Username string `json:"username"`
}

// FetchDisplayName resolves igsid's display name for tenantID, preferring
// the profile's "name" over its "username". Returns ErrProfileFetchFailed
// (wrapped) on a missing token, transport error, non-2xx response, or an
// empty name/username — the caller treats any error as "no name
// available" and proceeds with the existing fallback, never failing the
// inbound delivery on this account.
func (p *ProfileFetcher) FetchDisplayName(ctx context.Context, tenantID uuid.UUID, igsid string) (string, error) {
	igsid = strings.TrimSpace(igsid)
	if igsid == "" {
		return "", errors.New("instagram: igsid must not be empty")
	}
	token, err := p.tokenLookup(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("%w: token lookup: %v", ErrProfileFetchFailed, err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("%w: no access token for tenant", ErrProfileFetchFailed)
	}

	v := url.Values{}
	v.Set("fields", "name,username")
	v.Set("access_token", token)
	target := strings.TrimRight(p.baseURL, "/") + "/" + igsid + "?" + v.Encode()

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
	if name := strings.TrimSpace(out.Name); name != "" {
		return name, nil
	}
	if username := strings.TrimSpace(out.Username); username != "" {
		return username, nil
	}
	return "", fmt.Errorf("%w: empty name/username in response", ErrProfileFetchFailed)
}
