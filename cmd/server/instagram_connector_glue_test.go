package main

import (
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

func TestBuildInstagramConnector_NilWhenAppIDMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramConnector(nil, func(k string) string {
		if k == instagram.EnvInstagramAppSecret {
			return "s"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil connector when META_INSTAGRAM_APP_ID unset")
	}
}

func TestBuildInstagramConnector_NilWhenAppSecretMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramConnector(nil, func(k string) string {
		if k == envInstagramAppID {
			return "app123"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil connector when META_APP_SECRET unset")
	}
}

func TestBuildInstagramConnector_BuiltWhenConfigured(t *testing.T) {
	t.Parallel()
	got := buildInstagramConnector(nil, func(k string) string {
		switch k {
		case envInstagramAppID:
			return "app123"
		case instagram.EnvInstagramAppSecret:
			return "s"
		}
		return ""
	})
	if got == nil {
		t.Fatal("expected non-nil connector when both env vars are set")
	}
	url, err := got.AuthorizeURL(uuid.New(), "https://acme.example.com/webhooks/instagram/oauth/callback")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	if url == "" {
		t.Fatal("expected non-empty authorize URL")
	}
}
