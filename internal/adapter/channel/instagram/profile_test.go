package instagram_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channel/instagram"
)

func TestNewProfileFetcher_NilLookupErrors(t *testing.T) {
	t.Parallel()
	_, err := instagram.NewProfileFetcher(nil)
	if err == nil {
		t.Fatal("expected error for nil token lookup")
	}
}

func fixedTokenLookup(token string, err error) instagram.TokenLookup {
	return func(context.Context, uuid.UUID) (string, error) { return token, err }
}

func TestFetchDisplayName_PrefersNameOverUsername(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("access_token"); got != "tok" {
			t.Errorf("access_token: got %q", got)
		}
		if got := r.URL.Query().Get("fields"); got != "name,username" {
			t.Errorf("fields: got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"Maria Silva","username":"maria.silva"}`))
	}))
	defer srv.Close()

	p, err := instagram.NewProfileFetcher(fixedTokenLookup("tok", nil), instagram.WithProfileBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	name, err := p.FetchDisplayName(context.Background(), uuid.New(), "igsid123")
	if err != nil {
		t.Fatalf("FetchDisplayName: %v", err)
	}
	if name != "Maria Silva" {
		t.Fatalf("name: got %q want %q", name, "Maria Silva")
	}
}

func TestFetchDisplayName_FallsBackToUsername(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"username":"maria.silva"}`))
	}))
	defer srv.Close()

	p, err := instagram.NewProfileFetcher(fixedTokenLookup("tok", nil), instagram.WithProfileBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	name, err := p.FetchDisplayName(context.Background(), uuid.New(), "igsid123")
	if err != nil {
		t.Fatalf("FetchDisplayName: %v", err)
	}
	if name != "maria.silva" {
		t.Fatalf("name: got %q want %q", name, "maria.silva")
	}
}

func TestFetchDisplayName_EmptyTokenErrors(t *testing.T) {
	t.Parallel()
	p, err := instagram.NewProfileFetcher(fixedTokenLookup("", nil))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	if _, err := p.FetchDisplayName(context.Background(), uuid.New(), "igsid123"); !errors.Is(err, instagram.ErrProfileFetchFailed) {
		t.Fatalf("expected ErrProfileFetchFailed, got %v", err)
	}
}

func TestFetchDisplayName_TokenLookupErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	p, err := instagram.NewProfileFetcher(fixedTokenLookup("", boom))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	if _, err := p.FetchDisplayName(context.Background(), uuid.New(), "igsid123"); !errors.Is(err, instagram.ErrProfileFetchFailed) {
		t.Fatalf("expected ErrProfileFetchFailed, got %v", err)
	}
}

func TestFetchDisplayName_NonSuccessStatusErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"no permission"}}`))
	}))
	defer srv.Close()

	p, err := instagram.NewProfileFetcher(fixedTokenLookup("tok", nil), instagram.WithProfileBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	if _, err := p.FetchDisplayName(context.Background(), uuid.New(), "igsid123"); !errors.Is(err, instagram.ErrProfileFetchFailed) {
		t.Fatalf("expected ErrProfileFetchFailed, got %v", err)
	}
}

func TestFetchDisplayName_EmptyIGSIDErrors(t *testing.T) {
	t.Parallel()
	p, err := instagram.NewProfileFetcher(fixedTokenLookup("tok", nil))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	if _, err := p.FetchDisplayName(context.Background(), uuid.New(), "  "); err == nil {
		t.Fatal("expected error for empty igsid")
	}
}
