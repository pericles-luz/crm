// Package featureflag exposes the customdomain.ask_enabled global
// kill-switch (SIN-62243 F45). Default is ON; setting the env var
// CUSTOMDOMAIN_ASK_ENABLED to "false" / "0" / "off" turns the on-demand TLS
// path off without a deploy. Issuances already in flight inside Caddy
// continue independently — we only refuse new ones.
package featureflag

import (
	"context"
	"strings"
	"sync/atomic"
)

// EnvKey is the env-var name the EnvFlag adapter reads at startup. Kept
// public so cmd/server can document it in --help and operations can grep
// for it across docs.
const EnvKey = "CUSTOMDOMAIN_ASK_ENABLED"

// EnvFlag is the production FeatureFlag implementation. The value is
// captured once at construction (NewFromEnv); Reload lets ops flip it
// without a deploy by signalling the process to re-read the env. Atomic
// updates keep concurrent Ask handlers consistent.
type EnvFlag struct {
	enabled atomic.Bool
}

// NewFromEnv builds an EnvFlag from a getenv lookup. Empty / unset reads
// keep the default ON (the F45 acceptance criterion: feature flag default
// is true). Any value matching offValues turns it OFF.
func NewFromEnv(getenv func(string) string) *EnvFlag {
	f := &EnvFlag{}
	f.refresh(getenv)
	return f
}

// AskEnabled is the FeatureFlag port method. The signature returns an error
// so future remote-flag adapters can surface failures; the env-backed impl
// never errors.
func (f *EnvFlag) AskEnabled(_ context.Context) (bool, error) {
	return f.enabled.Load(), nil
}

// Reload re-reads getenv and atomically updates the flag. Call this from a
// SIGHUP handler in cmd/server when ops want to flip it without a restart.
func (f *EnvFlag) Reload(getenv func(string) string) {
	f.refresh(getenv)
}

func (f *EnvFlag) refresh(getenv func(string) string) {
	if getenv == nil {
		f.enabled.Store(true)
		return
	}
	v := strings.ToLower(strings.TrimSpace(getenv(EnvKey)))
	if v == "" {
		f.enabled.Store(true) // unset == default-on
		return
	}
	switch v {
	case "false", "0", "off", "no", "disabled":
		f.enabled.Store(false)
	default:
		f.enabled.Store(true)
	}
}
