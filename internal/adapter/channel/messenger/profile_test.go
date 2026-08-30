package messenger_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channel/messenger"
)

func TestNewProfileFetcher_EmptyTokenErrors(t *testing.T) {
	t.Parallel()
	_, err := messenger.NewProfileFetcher("")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestFetchDisplayName_JoinsFirstAndLastName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("access_token"); got != "tok" {
			t.Errorf("access_token: got %q", got)
		}
		if got := r.URL.Query().Get("fields"); got != "first_name,last_name" {
			t.Errorf("fields: got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"first_name":"Maria","last_name":"Silva"}`))
	}))
	defer srv.Close()

	p, err := messenger.NewProfileFetcher("tok", messenger.WithProfileBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	name, err := p.FetchDisplayName(context.Background(), "psid123")
	if err != nil {
		t.Fatalf("FetchDisplayName: %v", err)
	}
	if name != "Maria Silva" {
		t.Fatalf("name: got %q want %q", name, "Maria Silva")
	}
}

func TestFetchDisplayName_OnlyFirstName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"first_name":"Maria"}`))
	}))
	defer srv.Close()

	p, err := messenger.NewProfileFetcher("tok", messenger.WithProfileBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	name, err := p.FetchDisplayName(context.Background(), "psid123")
	if err != nil {
		t.Fatalf("FetchDisplayName: %v", err)
	}
	if name != "Maria" {
		t.Fatalf("name: got %q want %q", name, "Maria")
	}
}

func TestFetchDisplayName_EmptyNameErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p, err := messenger.NewProfileFetcher("tok", messenger.WithProfileBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	if _, err := p.FetchDisplayName(context.Background(), "psid123"); !errors.Is(err, messenger.ErrProfileFetchFailed) {
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

	p, err := messenger.NewProfileFetcher("tok", messenger.WithProfileBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	if _, err := p.FetchDisplayName(context.Background(), "psid123"); !errors.Is(err, messenger.ErrProfileFetchFailed) {
		t.Fatalf("expected ErrProfileFetchFailed, got %v", err)
	}
}

func TestFetchDisplayName_EmptyPSIDErrors(t *testing.T) {
	t.Parallel()
	p, err := messenger.NewProfileFetcher("tok")
	if err != nil {
		t.Fatalf("NewProfileFetcher: %v", err)
	}
	if _, err := p.FetchDisplayName(context.Background(), "  "); err == nil {
		t.Fatal("expected error for empty psid")
	}
}
