package main

import (
	"strings"
	"testing"
	"time"
)

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig_DefaultsWhenEmpty(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(mapEnv(nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := defaultConfig()
	if cfg.HTTPAddr != want.HTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, want.HTTPAddr)
	}
	if cfg.WebhookV2Enabled {
		t.Error("WebhookV2Enabled = true, want false (default off)")
	}
	if got, want := strings.Join(cfg.MetaChannels, ","), "whatsapp,instagram,facebook"; got != want {
		t.Errorf("MetaChannels = %q, want %q", got, want)
	}
	if cfg.NATSStreamDuplicatesWindow != time.Hour {
		t.Errorf("NATSStreamDuplicatesWindow = %v, want 1h", cfg.NATSStreamDuplicatesWindow)
	}
	if !cfg.NATSValidateStream {
		t.Error("NATSValidateStream = false, want true (default on)")
	}
	if cfg.ReconcilerTickEvery != 30*time.Second {
		t.Errorf("ReconcilerTickEvery = %v, want 30s", cfg.ReconcilerTickEvery)
	}
}

func TestLoadConfig_OverridesAllKnobs(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(mapEnv(map[string]string{
		"HTTP_ADDR":                    ":9999",
		"WEBHOOK_SECURITY_V2_ENABLED":  "true",
		"META_APP_SECRET":              "topsecret",
		"META_CHANNELS":                "whatsapp, , facebook ",
		"DATABASE_URL":                 "postgres://localhost/db",
		"NATS_STREAM_NAME":             "WB2",
		"NATS_SUBJECT_PREFIX":          "wh.",
		"NATS_STREAM_DUPLICATES_WINDOW": "2h",
		"NATS_VALIDATE_STREAM":         "false",
		"RECONCILER_TICK_EVERY":        "10s",
		"RECONCILER_STALE_AFTER":       "30s",
		"RECONCILER_ALERT_AFTER":       "2h",
		"RECONCILER_BATCH_SIZE":        "50",
		"RECONCILER_HEALTH_STALENESS":  "1m",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if !cfg.WebhookV2Enabled {
		t.Error("WebhookV2Enabled should be true")
	}
	if cfg.MetaAppSecret != "topsecret" {
		t.Errorf("MetaAppSecret = %q", cfg.MetaAppSecret)
	}
	if got, want := strings.Join(cfg.MetaChannels, ","), "whatsapp,facebook"; got != want {
		t.Errorf("MetaChannels = %q, want %q (whitespace/empties trimmed)", got, want)
	}
	if cfg.DatabaseURL != "postgres://localhost/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.NATSStreamName != "WB2" || cfg.NATSSubjectPrefix != "wh." {
		t.Errorf("NATS stream name/prefix = %q/%q", cfg.NATSStreamName, cfg.NATSSubjectPrefix)
	}
	if cfg.NATSStreamDuplicatesWindow != 2*time.Hour {
		t.Errorf("NATSStreamDuplicatesWindow = %v", cfg.NATSStreamDuplicatesWindow)
	}
	if cfg.NATSValidateStream {
		t.Error("NATSValidateStream should be false (override)")
	}
	if cfg.ReconcilerTickEvery != 10*time.Second {
		t.Errorf("ReconcilerTickEvery = %v", cfg.ReconcilerTickEvery)
	}
	if cfg.ReconcilerBatchSize != 50 {
		t.Errorf("ReconcilerBatchSize = %d", cfg.ReconcilerBatchSize)
	}
	if cfg.ReconcilerHealthStaleness != time.Minute {
		t.Errorf("ReconcilerHealthStaleness = %v", cfg.ReconcilerHealthStaleness)
	}
}

func TestLoadConfig_RequiresMetaAppSecretWhenFlagOn(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{
		"WEBHOOK_SECURITY_V2_ENABLED": "true",
		"DATABASE_URL":                "postgres://x",
	}))
	if err == nil || !strings.Contains(err.Error(), "META_APP_SECRET") {
		t.Fatalf("err = %v, want META_APP_SECRET error", err)
	}
}

func TestLoadConfig_RequiresDatabaseURLWhenFlagOn(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{
		"WEBHOOK_SECURITY_V2_ENABLED": "true",
		"META_APP_SECRET":             "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v, want DATABASE_URL error", err)
	}
}

func TestLoadConfig_RequiresNonEmptyChannelsWhenFlagOn(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{
		"WEBHOOK_SECURITY_V2_ENABLED": "true",
		"META_APP_SECRET":             "x",
		"DATABASE_URL":                "postgres://x",
		"META_CHANNELS":               " , ",
	}))
	if err == nil || !strings.Contains(err.Error(), "META_CHANNELS") {
		t.Fatalf("err = %v, want META_CHANNELS error", err)
	}
}

func TestLoadConfig_BadBoolReturnsError(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{
		"WEBHOOK_SECURITY_V2_ENABLED": "yesplease",
	}))
	if err == nil {
		t.Fatal("expected error for bad bool")
	}
}

func TestLoadConfig_BadDurationReturnsError(t *testing.T) {
	t.Parallel()
	keys := []string{
		"NATS_STREAM_DUPLICATES_WINDOW",
		"RECONCILER_TICK_EVERY",
		"RECONCILER_STALE_AFTER",
		"RECONCILER_ALERT_AFTER",
		"RECONCILER_HEALTH_STALENESS",
	}
	for _, k := range keys {
		k := k
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			_, err := loadConfig(mapEnv(map[string]string{k: "not-a-duration"}))
			if err == nil {
				t.Fatalf("%s: expected error", k)
			}
		})
	}
}

func TestLoadConfig_BadIntReturnsError(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{"RECONCILER_BATCH_SIZE": "fifty"}))
	if err == nil {
		t.Fatal("expected error for bad int")
	}
}

func TestLoadConfig_BadValidateStreamBool(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{"NATS_VALIDATE_STREAM": "maybe"}))
	if err == nil {
		t.Fatal("expected error for bad bool")
	}
}

func TestParseChannels_TrimsAndDropsEmpties(t *testing.T) {
	t.Parallel()
	got := parseChannels("a, b ,, c, ")
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseChannels = %v, want %v", got, want)
	}
}

func TestLoadConfig_NATSConnectionKnobs(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(mapEnv(map[string]string{
		"NATS_URL":              "tls://nats.example.com:4222",
		"NATS_CLIENT_NAME":      "crm-prod",
		"NATS_CREDS_FILE":       "/etc/nats/creds",
		"NATS_TLS_CA_FILE":      "/etc/nats/ca.pem",
		"NATS_TLS_CERT_FILE":    "/etc/nats/client.crt",
		"NATS_TLS_KEY_FILE":     "/etc/nats/client.key",
		"NATS_TLS_SERVER_NAME":  "nats.example.com",
		"NATS_RECONNECT_WAIT":   "5s",
		"NATS_MAX_RECONNECTS":   "30",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.NATSURL != "tls://nats.example.com:4222" {
		t.Errorf("NATSURL = %q", cfg.NATSURL)
	}
	if cfg.NATSClientName != "crm-prod" {
		t.Errorf("NATSClientName = %q", cfg.NATSClientName)
	}
	if cfg.NATSCredsFile != "/etc/nats/creds" {
		t.Errorf("NATSCredsFile = %q", cfg.NATSCredsFile)
	}
	if cfg.NATSTLSCAFile != "/etc/nats/ca.pem" || cfg.NATSTLSCertFile != "/etc/nats/client.crt" || cfg.NATSTLSKeyFile != "/etc/nats/client.key" {
		t.Errorf("TLS files = %q/%q/%q", cfg.NATSTLSCAFile, cfg.NATSTLSCertFile, cfg.NATSTLSKeyFile)
	}
	if cfg.NATSTLSServerName != "nats.example.com" {
		t.Errorf("NATSTLSServerName = %q", cfg.NATSTLSServerName)
	}
	if cfg.NATSReconnectWait != 5*time.Second {
		t.Errorf("NATSReconnectWait = %v", cfg.NATSReconnectWait)
	}
	if cfg.NATSMaxReconnects != 30 {
		t.Errorf("NATSMaxReconnects = %d", cfg.NATSMaxReconnects)
	}
}

func TestLoadConfig_NATSDefaultsAreLocalhostFriendly(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(mapEnv(nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.NATSURL != "nats://localhost:4222" {
		t.Errorf("NATSURL default = %q, want nats://localhost:4222", cfg.NATSURL)
	}
	if cfg.NATSClientName != "crm-server" {
		t.Errorf("NATSClientName default = %q", cfg.NATSClientName)
	}
	if cfg.NATSReconnectWait != 2*time.Second {
		t.Errorf("NATSReconnectWait default = %v", cfg.NATSReconnectWait)
	}
	if cfg.NATSMaxReconnects != -1 {
		t.Errorf("NATSMaxReconnects default = %d, want -1 (infinite)", cfg.NATSMaxReconnects)
	}
}

func TestValidateFlagOnInvariants_RequiresNATSURL(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.MetaAppSecret = "x"
	cfg.DatabaseURL = "postgres://x"
	cfg.NATSURL = ""
	if err := validateFlagOnInvariants(cfg); err == nil || !strings.Contains(err.Error(), "NATS_URL") {
		t.Fatalf("err = %v, want NATS_URL error", err)
	}
}

func TestLoadConfig_TLSCertAndKeyMustBePaired(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{
		"WEBHOOK_SECURITY_V2_ENABLED": "true",
		"META_APP_SECRET":             "x",
		"DATABASE_URL":                "postgres://x",
		"NATS_TLS_CERT_FILE":          "/etc/nats/client.crt",
	}))
	if err == nil || !strings.Contains(err.Error(), "NATS_TLS_CERT_FILE") {
		t.Fatalf("err = %v, want pairing error", err)
	}
}

func TestLoadConfig_BadNATSReconnectWaitReturnsError(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{"NATS_RECONNECT_WAIT": "not-a-duration"}))
	if err == nil {
		t.Fatal("expected error for bad duration")
	}
}

func TestLoadConfig_BadNATSMaxReconnectsReturnsError(t *testing.T) {
	t.Parallel()
	_, err := loadConfig(mapEnv(map[string]string{"NATS_MAX_RECONNECTS": "lots"}))
	if err == nil {
		t.Fatal("expected error for bad int")
	}
}
