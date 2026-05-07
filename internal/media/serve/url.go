package serve

import (
	"strings"

	"github.com/google/uuid"
)

// URLBuilder renders absolute URLs into the cookieless static origin.
// Templates (templ helpers, html/template funcs) embed this once at
// init and call MediaURL/LogoURL when emitting <img src=...>. Building
// URLs in Go (rather than string-concatenating in the template) keeps
// the host name configurable per-environment and prevents accidental
// drift between the Caddy block and the rendered HTML.
type URLBuilder struct {
	// origin is the scheme+host without a trailing slash, e.g.
	// "https://static.crm.example.com".
	origin string
}

// NewURLBuilder normalizes the origin (trims trailing slash) and rejects
// empty or relative origins, since rendering a same-origin URL out of
// the static origin would defeat the cookieless isolation guarantee.
func NewURLBuilder(origin string) (URLBuilder, error) {
	o := strings.TrimRight(origin, "/")
	if o == "" {
		return URLBuilder{}, errBadOrigin
	}
	if !strings.HasPrefix(o, "https://") && !strings.HasPrefix(o, "http://") {
		return URLBuilder{}, errBadOrigin
	}
	return URLBuilder{origin: o}, nil
}

// LogoURL renders the URL for `GET /t/{tenantID}/logo`. UUIDs already
// stringify to a fixed safe charset so no escaping is needed.
func (b URLBuilder) LogoURL(tenantID uuid.UUID) string {
	return b.origin + "/t/" + tenantID.String() + "/logo"
}

// MediaURL renders the URL for `GET /t/{tenantID}/m/{hash}`. The hash
// is expected to be lowercase hex (SHA-256); it is passed through
// unchanged because the handler's validHexHash is the authoritative
// gatekeeper at request time.
func (b URLBuilder) MediaURL(tenantID uuid.UUID, hash string) string {
	return b.origin + "/t/" + tenantID.String() + "/m/" + hash
}

// errBadOrigin is the sentinel for an origin that cannot be used to
// render absolute static URLs.
var errBadOrigin = stringError("serve: origin must be an absolute http(s) URL")

type stringError string

func (e stringError) Error() string { return string(e) }
