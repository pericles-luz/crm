package redis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	aicache "github.com/pericles-luz/crm/internal/ai/cache"
	"github.com/pericles-luz/crm/internal/ai/port"

	redisadapter "github.com/pericles-luz/crm/internal/ai/adapter/redis"
)

type fakeCmdable struct {
	getKey  string
	getResp *goredis.StringCmd
	setCall struct {
		key   string
		value any
		ttl   time.Duration
		resp  *goredis.StatusCmd
	}
	delKeys []string
	delResp *goredis.IntCmd
}

func (f *fakeCmdable) Get(_ context.Context, key string) *goredis.StringCmd {
	f.getKey = key
	return f.getResp
}

func (f *fakeCmdable) Set(_ context.Context, key string, value any, ttl time.Duration) *goredis.StatusCmd {
	f.setCall.key = key
	f.setCall.value = value
	f.setCall.ttl = ttl
	return f.setCall.resp
}

func (f *fakeCmdable) Del(_ context.Context, keys ...string) *goredis.IntCmd {
	f.delKeys = append([]string{}, keys...)
	return f.delResp
}

func newStringCmdValue(t *testing.T, value string) *goredis.StringCmd {
	t.Helper()
	cmd := goredis.NewStringCmd(context.Background(), "GET")
	cmd.SetVal(value)
	return cmd
}

func newStringCmdErr(t *testing.T, err error) *goredis.StringCmd {
	t.Helper()
	cmd := goredis.NewStringCmd(context.Background(), "GET")
	cmd.SetErr(err)
	return cmd
}

func newStatusCmdOK(t *testing.T) *goredis.StatusCmd {
	t.Helper()
	cmd := goredis.NewStatusCmd(context.Background(), "SET")
	cmd.SetVal("OK")
	return cmd
}

func newStatusCmdErr(t *testing.T, err error) *goredis.StatusCmd {
	t.Helper()
	cmd := goredis.NewStatusCmd(context.Background(), "SET")
	cmd.SetErr(err)
	return cmd
}

func newIntCmdVal(t *testing.T, n int64) *goredis.IntCmd {
	t.Helper()
	cmd := goredis.NewIntCmd(context.Background(), "DEL")
	cmd.SetVal(n)
	return cmd
}

func newIntCmdErr(t *testing.T, err error) *goredis.IntCmd {
	t.Helper()
	cmd := goredis.NewIntCmd(context.Background(), "DEL")
	cmd.SetErr(err)
	return cmd
}

func TestCacheGet_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCmdable{getResp: newStringCmdValue(t, "payload")}
	c := redisadapter.New(fake)

	key, err := aicache.TenantKey("acme", "conv-1", "msg-1")
	if err != nil {
		t.Fatalf("TenantKey: %v", err)
	}
	got, err := c.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("Get value = %q, want %q", string(got), "payload")
	}
	if fake.getKey != "tenant:acme:ai:summary:conv-1:msg-1" {
		t.Fatalf("redis Get called with key %q", fake.getKey)
	}
}

func TestCacheGet_MissReturnsSentinel(t *testing.T) {
	t.Parallel()
	fake := &fakeCmdable{getResp: newStringCmdErr(t, goredis.Nil)}
	c := redisadapter.New(fake)

	key, _ := aicache.TenantKey("acme", "conv-1", "msg-1")
	_, err := c.Get(context.Background(), key)
	if !errors.Is(err, port.ErrCacheMiss) {
		t.Fatalf("Get on miss = %v, want ErrCacheMiss", err)
	}
}

func TestCacheGet_WrapsInfraError(t *testing.T) {
	t.Parallel()
	infra := errors.New("boom")
	fake := &fakeCmdable{getResp: newStringCmdErr(t, infra)}
	c := redisadapter.New(fake)

	key, _ := aicache.TenantKey("acme", "conv-1", "msg-1")
	_, err := c.Get(context.Background(), key)
	if err == nil {
		t.Fatal("Get on infra error returned nil")
	}
	if errors.Is(err, port.ErrCacheMiss) {
		t.Fatal("infra error must not be reported as cache miss")
	}
	if !errors.Is(err, infra) {
		t.Fatalf("Get error = %v, want wrapping infra error", err)
	}
}

func TestCacheGet_RejectsZeroKey(t *testing.T) {
	t.Parallel()
	fake := &fakeCmdable{getResp: newStringCmdValue(t, "ignored")}
	c := redisadapter.New(fake)
	if _, err := c.Get(context.Background(), aicache.Key{}); err == nil {
		t.Fatal("Get with zero key returned nil error")
	}
	if fake.getKey != "" {
		t.Fatalf("redis Get must not be called with zero key, got %q", fake.getKey)
	}
}

func TestCacheSet_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCmdable{}
	fake.setCall.resp = newStatusCmdOK(t)
	c := redisadapter.New(fake)

	key, _ := aicache.SystemKey("summary", "conv-1", "msg-1")
	if err := c.Set(context.Background(), key, []byte("body"), 30*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if fake.setCall.key != "system:ai:summary:conv-1:msg-1" {
		t.Fatalf("redis Set key = %q", fake.setCall.key)
	}
	if got, ok := fake.setCall.value.([]byte); !ok || string(got) != "body" {
		t.Fatalf("redis Set value = %#v", fake.setCall.value)
	}
	if fake.setCall.ttl != 30*time.Second {
		t.Fatalf("redis Set ttl = %v", fake.setCall.ttl)
	}
}

func TestCacheSet_WrapsError(t *testing.T) {
	t.Parallel()
	infra := errors.New("nope")
	fake := &fakeCmdable{}
	fake.setCall.resp = newStatusCmdErr(t, infra)
	c := redisadapter.New(fake)

	key, _ := aicache.TenantKey("acme", "c", "m")
	err := c.Set(context.Background(), key, []byte("b"), time.Minute)
	if !errors.Is(err, infra) {
		t.Fatalf("Set error = %v, want wrapping infra", err)
	}
}

func TestCacheSet_RejectsZeroKey(t *testing.T) {
	t.Parallel()
	fake := &fakeCmdable{}
	fake.setCall.resp = newStatusCmdOK(t)
	c := redisadapter.New(fake)
	if err := c.Set(context.Background(), aicache.Key{}, []byte("b"), time.Minute); err == nil {
		t.Fatal("Set with zero key returned nil error")
	}
	if fake.setCall.key != "" {
		t.Fatalf("redis Set must not be called with zero key, got %q", fake.setCall.key)
	}
}

func TestCacheDel_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCmdable{delResp: newIntCmdVal(t, 1)}
	c := redisadapter.New(fake)
	key, _ := aicache.TenantKey("acme", "c", "m")
	if err := c.Del(context.Background(), key); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if len(fake.delKeys) != 1 || fake.delKeys[0] != "tenant:acme:ai:summary:c:m" {
		t.Fatalf("redis Del keys = %v", fake.delKeys)
	}
}

func TestCacheDel_WrapsError(t *testing.T) {
	t.Parallel()
	infra := errors.New("kaboom")
	fake := &fakeCmdable{delResp: newIntCmdErr(t, infra)}
	c := redisadapter.New(fake)
	key, _ := aicache.TenantKey("acme", "c", "m")
	err := c.Del(context.Background(), key)
	if !errors.Is(err, infra) {
		t.Fatalf("Del error = %v, want wrapping infra", err)
	}
}

func TestCacheDel_RejectsZeroKey(t *testing.T) {
	t.Parallel()
	fake := &fakeCmdable{delResp: newIntCmdVal(t, 0)}
	c := redisadapter.New(fake)
	if err := c.Del(context.Background(), aicache.Key{}); err == nil {
		t.Fatal("Del with zero key returned nil error")
	}
	if fake.delKeys != nil {
		t.Fatalf("redis Del must not be called with zero key, got %v", fake.delKeys)
	}
}

func TestCacheImplementsPort(t *testing.T) {
	t.Parallel()
	var _ port.Cache = (*redisadapter.Cache)(nil)
}
