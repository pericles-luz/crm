package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// SIN-62334 F53 boot-failure tests — these live in their own file so
// the existing customdomain_wire_test.go is untouched. The CTO requested
// a unit test that the wire-validation path returns an error when
// CUSTOM_DOMAIN_UI_ENABLED=1 and Redis is not configured.

func TestEnrollmentRedisRequired_FlagOff_NilError(t *testing.T) {
	t.Parallel()
	getenv := func(string) string { return "" }
	if err := EnrollmentRedisRequired(getenv); err != nil {
		t.Fatalf("flag off: expected nil, got %v", err)
	}
}

func TestEnrollmentRedisRequired_FlagOnRedisSet_NilError(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case envCustomDomainRedisURL:
			return "redis://localhost:6379"
		}
		return ""
	}
	if err := EnrollmentRedisRequired(getenv); err != nil {
		t.Fatalf("flag on + REDIS_URL set: expected nil, got %v", err)
	}
}

func TestEnrollmentRedisRequired_FlagOnRedisUnset_HardErrors(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envCustomDomainUI {
			return "1"
		}
		return ""
	}
	err := EnrollmentRedisRequired(getenv)
	if err == nil {
		t.Fatal("flag on + REDIS_URL unset: expected ErrEnrollmentRedisRequired, got nil")
	}
	if !errors.Is(err, ErrEnrollmentRedisRequired) {
		t.Fatalf("error is not ErrEnrollmentRedisRequired: %v", err)
	}
}

// fakeRedisClient is a minimal in-memory adapter for the Redis ops the
// enrollment gate uses. It mirrors the documented-fake pattern from
// internal/customdomain/ratelimit/sliding's fakeServer (CTO-approved:
// matches Redis semantics, not canned stubs). Used to drive the
// wire-up end-to-end without a real Redis.
type fakeRedisClient struct {
	mu      sync.Mutex
	zsets   map[string][]zentry
	strings map[string]struct{}
}

type zentry struct {
	score  int64
	member string
}

func newFakeRedisClient() *fakeRedisClient {
	return &fakeRedisClient{
		zsets:   map[string][]zentry{},
		strings: map[string]struct{}{},
	}
}

func (f *fakeRedisClient) Eval(_ context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := &goredis.Cmd{}
	switch {
	case strings.Contains(script, "ZADD"):
		// rediswindow + redisstate-RecordFailure: count and record.
		if len(keys) != 1 || len(args) != 4 {
			cmd.SetErr(errors.New("fake: zadd arity"))
			return cmd
		}
		key := keys[0]
		score, _ := args[0].(int64)
		cutoff, _ := args[1].(int64)
		member, _ := args[2].(string)
		entries := f.zsets[key]
		kept := entries[:0]
		for _, e := range entries {
			if e.score > cutoff {
				kept = append(kept, e)
			}
		}
		kept = append(kept, zentry{score: score, member: member})
		f.zsets[key] = kept
		cmd.SetVal(int64(len(kept)))
	case strings.Contains(script, "EXISTS"):
		if _, ok := f.strings[keys[0]]; ok {
			cmd.SetVal(int64(1))
			return cmd
		}
		cmd.SetVal(int64(0))
	case strings.Contains(script, "DEL"):
		for _, k := range keys {
			delete(f.strings, k)
			delete(f.zsets, k)
		}
		cmd.SetVal(int64(1))
	case strings.Contains(script, "SET"):
		f.strings[keys[0]] = struct{}{}
		cmd.SetVal(int64(1))
	default:
		cmd.SetErr(errors.New("fake: unknown script"))
	}
	return cmd
}

func (f *fakeRedisClient) Close() error { return nil }

func TestBuildCustomDomainHandler_WiredAgainstRedisStub(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case "DATABASE_URL":
			return "postgres://example/db"
		case envCustomDomainCSRF:
			return strings.Repeat("a", 32)
		case envCustomDomainPrimary:
			return "exemplo.com"
		case envCustomDomainRedisURL:
			return "redis://example:6379"
		}
		return ""
	}
	dial := func(_ context.Context, _ string) (customDomainPool, error) {
		return &fakeCustomDomainPool{}, nil
	}
	resolver := func(_ func(string) string) interface{ Resolve() }(nil)
	_ = resolver
	redisDial := func(_ context.Context, _ string) (customDomainRedis, error) {
		return newFakeRedisClient(), nil
	}
	h, cleanup := buildCustomDomainHandlerWithRedis(context.Background(), getenv, dial, defaultCustomDomainResolverFactory, redisDial)
	if h == nil {
		t.Fatal("expected handler when Redis stub configured")
	}
	defer cleanup()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil)
	req.Header.Set(envCustomDomainTenantHd, uuid.New().String())
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBuildCustomDomainHandler_RedisDialError_FallsBackToNoop(t *testing.T) {
	t.Parallel()
	// When dial fails, the wire-up logs and returns nil (placeholder
	// path) so the existing dev/test paths keep working. Production
	// already errored at EnrollmentRedisRequired BEFORE reaching here,
	// so this branch is for tests that intentionally pass a failing
	// redisDial.
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case "DATABASE_URL":
			return "postgres://example/db"
		case envCustomDomainCSRF:
			return strings.Repeat("a", 32)
		case envCustomDomainRedisURL:
			return "redis://example:6379"
		}
		return ""
	}
	dial := func(_ context.Context, _ string) (customDomainPool, error) {
		return &fakeCustomDomainPool{}, nil
	}
	redisDial := func(_ context.Context, _ string) (customDomainRedis, error) {
		return nil, errors.New("ping refused")
	}
	h, cleanup := buildCustomDomainHandlerWithRedis(context.Background(), getenv, dial, defaultCustomDomainResolverFactory, redisDial)
	defer cleanup()
	if h != nil {
		t.Fatal("expected nil handler on Redis dial failure")
	}
}

func TestBuildEnrollmentGate_NilRedisFallsBackToPlaceholders(t *testing.T) {
	t.Parallel()
	gate, cleanup := buildEnrollmentGate(nil, nil)
	defer cleanup()
	if gate == nil {
		t.Fatal("nil gate when rdb is nil")
	}
	// Allow path: empty store → not denied; over-write the gate with a
	// real allow call to exercise the underlying enrollment.UseCase.
	dec := gate.Allow(context.Background(), uuid.New())
	if !dec.Allowed {
		t.Fatalf("placeholder gate denied: %+v", dec)
	}
}

func TestBuildEnrollmentGate_WithRedisAllowsFirstCall(t *testing.T) {
	t.Parallel()
	rdb := newFakeRedisClient()
	gate, cleanup := buildEnrollmentGate(&fakeCustomDomainPool{}, rdb)
	defer cleanup()
	dec := gate.Allow(context.Background(), uuid.New())
	if !dec.Allowed {
		t.Fatalf("first call denied: %+v", dec)
	}
}
