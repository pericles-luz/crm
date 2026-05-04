// Package policy reads the rate-limit YAML policy from disk and exposes the
// parsed rules. It owns the mapping between the on-disk schema (config/
// ratelimit.yaml) and the runtime types in internal/ratelimit / internal/web/
// middleware.
//
// The wiring code (cmd/server) is what binds the policy.Rule slice into
// middleware.Rule values — the policy package itself does not depend on
// net/http so it can be reused in CLI tools and migrations.
package policy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pericles-luz/crm/internal/ratelimit"
)

// File is the on-disk YAML document.
type File struct {
	Enabled bool   `yaml:"enabled"`
	Rules   []Rule `yaml:"rules"`
}

// Rule mirrors one entry under `rules:` in ratelimit.yaml.
type Rule struct {
	Endpoint   string    `yaml:"endpoint"`
	Bucket     string    `yaml:"bucket"`
	// Key encodes the extractor: "ip", "form:<field>", "header:<name>",
	// "ctx:<name>". The wiring code resolves the actual extractor function.
	Key        string    `yaml:"key"`
	Limit      LimitSpec `yaml:"limit"`
	FailClosed bool      `yaml:"fail_closed"`
}

// LimitSpec is the on-disk view of ratelimit.Limit.
type LimitSpec struct {
	Window time.Duration `yaml:"window"`
	Max    int           `yaml:"max"`
}

// AsLimit converts the on-disk spec into the domain type.
func (l LimitSpec) AsLimit() ratelimit.Limit {
	return ratelimit.Limit{Window: l.Window, Max: l.Max}
}

// LoadFile reads and validates a policy file from path.
func LoadFile(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ratelimit/policy: open %s: %w", path, err)
	}
	defer f.Close()
	return Decode(f)
}

// Decode parses a YAML stream and returns the validated policy. Validation
// errors include the offending rule index so misconfigurations surface
// during integration tests, not at the first rate-limited request.
func Decode(r io.Reader) (*File, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ratelimit/policy: read: %w", err)
	}
	var raw File
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("ratelimit/policy: decode: %w", err)
	}
	if err := raw.Validate(); err != nil {
		return nil, err
	}
	return &raw, nil
}

// Validate reports the first integrity violation in f.
func (f *File) Validate() error {
	for i, r := range f.Rules {
		if strings.TrimSpace(r.Endpoint) == "" {
			return fmt.Errorf("ratelimit/policy: rule[%d]: endpoint is required", i)
		}
		if strings.TrimSpace(r.Bucket) == "" {
			return fmt.Errorf("ratelimit/policy: rule[%d] (%s): bucket is required", i, r.Endpoint)
		}
		if strings.TrimSpace(r.Key) == "" {
			return fmt.Errorf("ratelimit/policy: rule[%d] (%s): key is required", i, r.Endpoint)
		}
		if !knownKeyKind(r.Key) {
			return fmt.Errorf("ratelimit/policy: rule[%d] (%s): unknown key %q (want ip|form:<field>|header:<name>|ctx:<name>)", i, r.Endpoint, r.Key)
		}
		if r.Limit.Window <= 0 {
			return fmt.Errorf("ratelimit/policy: rule[%d] (%s): limit.window must be > 0", i, r.Endpoint)
		}
		if r.Limit.Max <= 0 {
			return fmt.Errorf("ratelimit/policy: rule[%d] (%s): limit.max must be > 0", i, r.Endpoint)
		}
	}
	return nil
}

func knownKeyKind(key string) bool {
	if key == "ip" {
		return true
	}
	for _, prefix := range []string{"form:", "header:", "ctx:"} {
		if strings.HasPrefix(key, prefix) && len(key) > len(prefix) {
			return true
		}
	}
	return false
}
