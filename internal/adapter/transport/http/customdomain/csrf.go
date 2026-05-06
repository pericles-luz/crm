package customdomain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
)

// csrfTokenBytes is the per-request token length. 24 bytes ≈ 192 bits of
// entropy, more than enough for an HMAC-signed double-submit token.
const csrfTokenBytes = 24

// CSRFCookieName is the cookie that holds the random token. It is
// HttpOnly + Secure (in production) + SameSite=Lax. The form / HTMX
// header MUST submit the matching token (double-submit pattern).
const CSRFCookieName = "_csrf"

// CSRFFormField is the input name (and HTMX header name in lowercase)
// the handler looks for on every state-changing request.
const CSRFFormField = "_csrf"

// CSRFHeader is the HTTP header alternative HTMX uses
// (`hx-headers='{"X-CSRF-Token": "..."}'` in templates).
const CSRFHeader = "X-CSRF-Token"

// ErrCSRFInvalid is returned by VerifyCSRF when the request's token
// does not match the cookie. The handler renders a 403 and a PT-BR
// error in response.
var ErrCSRFInvalid = errors.New("csrf: token mismatch")

// CSRFConfig drives the middleware. Secret is 32+ bytes; it signs the
// HMAC over the token. Secure controls the cookie's Secure flag —
// production sets true, dev/local can leave false.
type CSRFConfig struct {
	Secret []byte
	Secure bool
}

// IssueCSRFToken sets the cookie and returns the token the template
// embeds. If the request already carries a valid cookie+HMAC pair it is
// reused so concurrent tabs do not invalidate each other.
func IssueCSRFToken(w http.ResponseWriter, r *http.Request, cfg CSRFConfig) (string, error) {
	if existing, err := r.Cookie(CSRFCookieName); err == nil && existing.Value != "" {
		if VerifyCSRFCookieValue(existing.Value, cfg.Secret) == nil {
			return existing.Value, nil
		}
	}
	tok, err := newCSRFToken(cfg.Secret)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	return tok, nil
}

// VerifyCSRF reads the submitted token (form field or header), the
// cookie, and the configured HMAC secret. Returns ErrCSRFInvalid on any
// mismatch; nil only when both are present, valid HMACs, and equal.
func VerifyCSRF(r *http.Request, cfg CSRFConfig) error {
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return ErrCSRFInvalid
	}
	if err := VerifyCSRFCookieValue(cookie.Value, cfg.Secret); err != nil {
		return ErrCSRFInvalid
	}
	submitted := r.Header.Get(CSRFHeader)
	if submitted == "" {
		submitted = r.FormValue(CSRFFormField)
	}
	if submitted == "" {
		return ErrCSRFInvalid
	}
	if !hmac.Equal([]byte(submitted), []byte(cookie.Value)) {
		return ErrCSRFInvalid
	}
	return nil
}

// VerifyCSRFCookieValue checks that v is a `<random>.<hmac>` string
// signed by secret. Returns ErrCSRFInvalid otherwise.
func VerifyCSRFCookieValue(v string, secret []byte) error {
	random, mac, err := splitCSRF(v)
	if err != nil {
		return ErrCSRFInvalid
	}
	expected := computeCSRFHMAC(random, secret)
	if !hmac.Equal(mac, expected) {
		return ErrCSRFInvalid
	}
	return nil
}

// newCSRFToken returns a fresh `<base64(random)>.<base64(hmac)>` string.
func newCSRFToken(secret []byte) (string, error) {
	var raw [csrfTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	random := base64.RawURLEncoding.EncodeToString(raw[:])
	mac := computeCSRFHMAC([]byte(random), secret)
	return random + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

func splitCSRF(v string) (random []byte, mac []byte, err error) {
	for i := 0; i < len(v); i++ {
		if v[i] == '.' {
			randomPart := []byte(v[:i])
			macPart, err := base64.RawURLEncoding.DecodeString(v[i+1:])
			if err != nil {
				return nil, nil, err
			}
			return randomPart, macPart, nil
		}
	}
	return nil, nil, ErrCSRFInvalid
}

func computeCSRFHMAC(random, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(random)
	return h.Sum(nil)
}
