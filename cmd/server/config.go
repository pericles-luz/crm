package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

// config bundles the runtime knobs the binary reads from environment.
// Defaults are *production-safe*: WebhookV2Enabled=false means a misdeployed
// container serves the stub handler, never the real pipeline (ADR §6
// Reversibilidade).
type config struct {
	HTTPAddr string

	WebhookV2Enabled bool

	MetaAppSecret string
	MetaChannels  []string

	DatabaseURL string

	NATSURL                    string
	NATSClientName             string
	NATSCredsFile              string
	NATSTLSCAFile              string
	NATSTLSCertFile            string
	NATSTLSKeyFile             string
	NATSTLSServerName          string
	NATSReconnectWait          time.Duration
	NATSMaxReconnects          int
	NATSStreamName             string
	NATSSubjectPrefix          string
	NATSStreamDuplicatesWindow time.Duration
	NATSValidateStream         bool

	ReconcilerTickEvery       time.Duration
	ReconcilerStaleAfter      time.Duration
	ReconcilerAlertAfter      time.Duration
	ReconcilerBatchSize       int
	ReconcilerHealthStaleness time.Duration
}

func defaultConfig() config {
	return config{
		HTTPAddr:                   defaultAddr,
		MetaChannels:               []string{"whatsapp", "instagram", "facebook"},
		NATSURL:                    "nats://localhost:4222",
		NATSClientName:             "crm-server",
		NATSReconnectWait:          2 * time.Second,
		NATSMaxReconnects:          -1,
		NATSStreamName:             "WEBHOOKS",
		NATSSubjectPrefix:          "webhooks.",
		NATSStreamDuplicatesWindow: nats.MinDuplicatesWindow,
		NATSValidateStream:         true,
		ReconcilerTickEvery:        30 * time.Second,
		ReconcilerStaleAfter:       time.Minute,
		ReconcilerAlertAfter:       time.Hour,
		ReconcilerBatchSize:        100,
		ReconcilerHealthStaleness:  90 * time.Second,
	}
}

// loadConfig reads cfg from getenv and validates inter-field constraints.
// Errors are explicit so the operator can spot the misconfigured key in
// the startup log.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := defaultConfig()

	if v := getenv("HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}

	enabled, err := parseBoolEnv("WEBHOOK_SECURITY_V2_ENABLED", getenv, false)
	if err != nil {
		return cfg, err
	}
	cfg.WebhookV2Enabled = enabled

	cfg.MetaAppSecret = getenv("META_APP_SECRET")
	if v := getenv("META_CHANNELS"); v != "" {
		cfg.MetaChannels = parseChannels(v)
	}
	cfg.DatabaseURL = getenv("DATABASE_URL")

	if v := getenv("NATS_URL"); v != "" {
		cfg.NATSURL = v
	}
	if v := getenv("NATS_CLIENT_NAME"); v != "" {
		cfg.NATSClientName = v
	}
	cfg.NATSCredsFile = getenv("NATS_CREDS_FILE")
	cfg.NATSTLSCAFile = getenv("NATS_TLS_CA_FILE")
	cfg.NATSTLSCertFile = getenv("NATS_TLS_CERT_FILE")
	cfg.NATSTLSKeyFile = getenv("NATS_TLS_KEY_FILE")
	cfg.NATSTLSServerName = getenv("NATS_TLS_SERVER_NAME")
	if d, ok, err := parseDurationEnv("NATS_RECONNECT_WAIT", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.NATSReconnectWait = d
	}
	if n, ok, err := parseIntEnv("NATS_MAX_RECONNECTS", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.NATSMaxReconnects = n
	}

	if v := getenv("NATS_STREAM_NAME"); v != "" {
		cfg.NATSStreamName = v
	}
	if v := getenv("NATS_SUBJECT_PREFIX"); v != "" {
		cfg.NATSSubjectPrefix = v
	}
	if d, ok, err := parseDurationEnv("NATS_STREAM_DUPLICATES_WINDOW", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.NATSStreamDuplicatesWindow = d
	}
	validate, err := parseBoolEnv("NATS_VALIDATE_STREAM", getenv, true)
	if err != nil {
		return cfg, err
	}
	cfg.NATSValidateStream = validate

	if d, ok, err := parseDurationEnv("RECONCILER_TICK_EVERY", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.ReconcilerTickEvery = d
	}
	if d, ok, err := parseDurationEnv("RECONCILER_STALE_AFTER", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.ReconcilerStaleAfter = d
	}
	if d, ok, err := parseDurationEnv("RECONCILER_ALERT_AFTER", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.ReconcilerAlertAfter = d
	}
	if n, ok, err := parseIntEnv("RECONCILER_BATCH_SIZE", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.ReconcilerBatchSize = n
	}
	if d, ok, err := parseDurationEnv("RECONCILER_HEALTH_STALENESS", getenv); err != nil {
		return cfg, err
	} else if ok {
		cfg.ReconcilerHealthStaleness = d
	}

	if cfg.WebhookV2Enabled {
		if err := validateFlagOnInvariants(cfg); err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}

// validateFlagOnInvariants enforces the cross-field requirements that
// only matter when WEBHOOK_SECURITY_V2_ENABLED=true. Extracted so tests
// can poke specific fields without re-deriving the env getter.
func validateFlagOnInvariants(cfg config) error {
	if cfg.MetaAppSecret == "" {
		return errors.New("META_APP_SECRET is required when WEBHOOK_SECURITY_V2_ENABLED=true")
	}
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required when WEBHOOK_SECURITY_V2_ENABLED=true")
	}
	if len(cfg.MetaChannels) == 0 {
		return errors.New("META_CHANNELS must list at least one channel")
	}
	if cfg.NATSURL == "" {
		return errors.New("NATS_URL is required when WEBHOOK_SECURITY_V2_ENABLED=true")
	}
	if (cfg.NATSTLSCertFile == "") != (cfg.NATSTLSKeyFile == "") {
		return errors.New("NATS_TLS_CERT_FILE and NATS_TLS_KEY_FILE must be set together")
	}
	return nil
}

func parseBoolEnv(key string, getenv func(string) string, def bool) (bool, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func parseDurationEnv(key string, getenv func(string) string) (time.Duration, bool, error) {
	v := getenv(key)
	if v == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", key, err)
	}
	return d, true, nil
}

func parseIntEnv(key string, getenv func(string) string) (int, bool, error) {
	v := getenv(key)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", key, err)
	}
	return n, true, nil
}

func parseChannels(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
