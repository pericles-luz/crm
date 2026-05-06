package featureflag_test

import (
	"context"
	"testing"

	"github.com/pericles-luz/crm/internal/customdomain/featureflag"
)

func mustGet(t *testing.T, f *featureflag.EnvFlag) bool {
	t.Helper()
	got, err := f.AskEnabled(context.Background())
	if err != nil {
		t.Fatalf("AskEnabled: %v", err)
	}
	return got
}

func TestEnvFlag_DefaultsToOn(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"unset":         "",
		"empty-string":  "",
		"true-explicit": "true",
		"on-ascii":      "on",
		"yes":           "yes",
		"1":             "1",
	}
	for name, val := range cases {
		val := val
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := featureflag.NewFromEnv(func(string) string { return val })
			if got := mustGet(t, f); !got {
				t.Fatalf("AskEnabled = false for %q, want true", val)
			}
		})
	}
}

func TestEnvFlag_OffValues(t *testing.T) {
	t.Parallel()
	off := []string{"false", "0", "off", "no", "disabled", "FALSE", "  off  "}
	for _, v := range off {
		v := v
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			f := featureflag.NewFromEnv(func(k string) string {
				if k == featureflag.EnvKey {
					return v
				}
				return ""
			})
			if got := mustGet(t, f); got {
				t.Fatalf("AskEnabled = true for %q, want false", v)
			}
		})
	}
}

func TestEnvFlag_NilGetenv_DefaultsOn(t *testing.T) {
	t.Parallel()
	f := featureflag.NewFromEnv(nil)
	if got := mustGet(t, f); !got {
		t.Fatalf("AskEnabled = false with nil getenv; want default ON")
	}
	// Reload(nil) is also safe.
	f.Reload(nil)
	if got := mustGet(t, f); !got {
		t.Fatalf("AskEnabled = false after Reload(nil); want default ON")
	}
}

func TestEnvFlag_ReloadFlipsValue(t *testing.T) {
	t.Parallel()
	val := "off"
	get := func(string) string { return val }

	f := featureflag.NewFromEnv(get)
	if mustGet(t, f) {
		t.Fatal("AskEnabled = true at construction; want false")
	}

	val = "true"
	f.Reload(get)
	if !mustGet(t, f) {
		t.Fatal("AskEnabled = false after Reload to true; want true")
	}
}
