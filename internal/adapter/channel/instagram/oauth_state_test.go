package instagram_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channel/instagram"
)

func TestOAuthState_RoundTrip(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-at-least-16-bytes")
	tenantID := uuid.New()

	state, err := instagram.SignOAuthState(secret, tenantID, 10*time.Minute)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	got, err := instagram.VerifyOAuthState(secret, state)
	if err != nil {
		t.Fatalf("VerifyOAuthState: %v", err)
	}
	if got != tenantID {
		t.Fatalf("got tenant %s, want %s", got, tenantID)
	}
}

func TestOAuthState_ExpiredRejected(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-at-least-16-bytes")
	state, err := instagram.SignOAuthState(secret, uuid.New(), -time.Minute)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	if _, err := instagram.VerifyOAuthState(secret, state); err == nil {
		t.Fatal("expected expired state to be rejected")
	}
}

func TestOAuthState_TamperedRejected(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-at-least-16-bytes")
	state, err := instagram.SignOAuthState(secret, uuid.New(), 10*time.Minute)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	tampered := state + "x"
	if _, err := instagram.VerifyOAuthState(secret, tampered); err == nil {
		t.Fatal("expected tampered state to be rejected")
	}
}

func TestOAuthState_WrongSecretRejected(t *testing.T) {
	t.Parallel()
	state, err := instagram.SignOAuthState([]byte("secret-a-16-bytes!!"), uuid.New(), 10*time.Minute)
	if err != nil {
		t.Fatalf("SignOAuthState: %v", err)
	}
	if _, err := instagram.VerifyOAuthState([]byte("secret-b-16-bytes!!"), state); err == nil {
		t.Fatal("expected mismatched secret to be rejected")
	}
}

func TestOAuthState_MalformedRejected(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-at-least-16-bytes")
	cases := []string{"", "no-dot-here", "not-base64!!!.also-not-base64!!!"}
	for _, s := range cases {
		if _, err := instagram.VerifyOAuthState(secret, s); err == nil {
			t.Errorf("state %q: expected error, got nil", s)
		}
	}
}

func TestOAuthState_EmptySecretRejected(t *testing.T) {
	t.Parallel()
	if _, err := instagram.SignOAuthState(nil, uuid.New(), time.Minute); err == nil {
		t.Fatal("expected SignOAuthState with empty secret to error")
	}
	if _, err := instagram.VerifyOAuthState(nil, "whatever"); err == nil {
		t.Fatal("expected VerifyOAuthState with empty secret to error")
	}
}
