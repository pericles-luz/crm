package main

import (
	"context"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

func TestBuildInstagramOAuthWiring_DisabledWhenAppIDMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramOAuthWiring(context.Background(), func(k string) string {
		if k == instagram.EnvAppSecret {
			return "s"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when META_INSTAGRAM_APP_ID unset")
	}
}

func TestBuildInstagramOAuthWiring_DisabledWhenAppSecretMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramOAuthWiring(context.Background(), func(k string) string {
		if k == envInstagramAppID {
			return "app123"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when META_APP_SECRET unset")
	}
}

func TestBuildInstagramOAuthWiring_DisabledWhenDSNMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramOAuthWiring(context.Background(), func(k string) string {
		switch k {
		case envInstagramAppID:
			return "app123"
		case instagram.EnvAppSecret:
			return "s"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when DATABASE_URL unset")
	}
}
