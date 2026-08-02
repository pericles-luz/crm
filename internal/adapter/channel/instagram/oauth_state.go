package instagram

// Signed OAuth "state" parameter for the Business Login for Instagram
// redirect. Deliberately stateless (no Redis/DB round trip): the
// callback may land on a different process instance than the one that
// issued the redirect, and it must resolve the tenant even if the
// browser drops cookies across the cross-site Instagram redirect. The
// shape mirrors internal/adapter/transport/http/customdomain/csrf.go's
// self-verifying `<random>.<hmac>` double-submit token, except the
// payload here is meaningful (tenantID|expiresAt) instead of random.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrOAuthStateInvalid is returned by VerifyOAuthState for any malformed,
// tampered, or expired state value.
var ErrOAuthStateInvalid = errors.New("instagram: oauth state invalid or expired")

// SignOAuthState encodes tenantID plus an expiry (now+ttl) into a
// tamper-evident state value for the Business Login redirect.
func SignOAuthState(secret []byte, tenantID uuid.UUID, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("instagram: oauth state secret must not be empty")
	}
	payload := fmt.Sprintf("%s|%d", tenantID.String(), time.Now().Add(ttl).Unix())
	mac := signState([]byte(payload), secret)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

// VerifyOAuthState checks the HMAC and expiry embedded in state and
// returns the tenant id it was signed for.
func VerifyOAuthState(secret []byte, state string) (uuid.UUID, error) {
	if len(secret) == 0 {
		return uuid.Nil, errors.New("instagram: oauth state secret must not be empty")
	}
	i := strings.IndexByte(state, '.')
	if i < 0 {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(state[:i])
	if err != nil {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	mac, err := base64.RawURLEncoding.DecodeString(state[i+1:])
	if err != nil {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	if !hmac.Equal(mac, signState(payloadRaw, secret)) {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	parts := strings.SplitN(string(payloadRaw), "|", 2)
	if len(parts) != 2 {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	tenantID, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	expUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	if time.Now().Unix() > expUnix {
		return uuid.Nil, ErrOAuthStateInvalid
	}
	return tenantID, nil
}

func signState(payload, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(payload)
	return h.Sum(nil)
}
