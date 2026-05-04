//go:build integration

// Run with: go test -tags=integration ./internal/ratelimit/adapter/redis/...
//
// This file uses testcontainers-go to spin up a real Redis 7. The unit
// tests in limiter_test.go cover the error and mapping paths against a
// fake Rediser; this integration test asserts the production path
// (real Lua script, real network) end-to-end so we don't ship a wrapper
// that drifts silently from redis_rate's contract.
package redis_test

import (
	"context"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/pericles-luz/crm/internal/ratelimit"
	redisadapter "github.com/pericles-luz/crm/internal/ratelimit/adapter/redis"
)

func TestIntegration_RealRedis_AllowsThenDenies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	rdb := goredis.NewClient(&goredis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { _ = rdb.Close() })

	lim := redisadapter.New(rdb)
	limit := ratelimit.Limit{Window: 5 * time.Second, Max: 2}

	for i := 1; i <= 2; i++ {
		dec, err := lim.Check(ctx, "integration:k", limit)
		if err != nil {
			t.Fatalf("call %d: err=%v", i, err)
		}
		if !dec.Allowed {
			t.Fatalf("call %d: must be allowed", i)
		}
	}
	dec, err := lim.Check(ctx, "integration:k", limit)
	if err != nil {
		t.Fatalf("3rd call err: %v", err)
	}
	if dec.Allowed {
		t.Fatal("3rd call must be denied (Max=2 already consumed)")
	}
	if dec.Retry <= 0 {
		t.Fatalf("denied Retry = %v, want > 0", dec.Retry)
	}
}
