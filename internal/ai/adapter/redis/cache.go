// Package redis implements port.Cache on top of github.com/redis/go-redis/v9.
//
// This is the only place under internal/ai/... that may import the redis
// client; the aicache analyzer (internal/lint/aicache) enforces this so that
// every cache access for the AI panel must travel through the typed
// port.Cache contract, which itself only accepts the opaque cache.Key.
package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	aicache "github.com/pericles-luz/crm/internal/ai/cache"
	"github.com/pericles-luz/crm/internal/ai/port"
)

// Cmdable is the narrow subset of the go-redis client surface that the AI
// cache adapter relies on. Declaring it here lets tests substitute a fake
// without dragging in a real redis server.
type Cmdable interface {
	Get(ctx context.Context, key string) *goredis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *goredis.StatusCmd
	Del(ctx context.Context, keys ...string) *goredis.IntCmd
}

// Cache is the redis-backed implementation of port.Cache. It keeps its
// dependency on Cmdable (not *goredis.Client) so callers can wire either the
// real client or a test double.
type Cache struct {
	client Cmdable
}

// New returns a Cache that reads and writes through the given Cmdable.
func New(client Cmdable) *Cache {
	return &Cache{client: client}
}

// errZeroKey is returned when the adapter receives a zero-value cache.Key
// (i.e. one that did not come from TenantKey or SystemKey). It is a defensive
// guard — the type system already prevents raw strings from arriving here,
// but a forgotten error check on the constructor would otherwise propagate a
// blank key into redis.
var errZeroKey = errors.New("ai/adapter/redis: refusing to operate on zero-value cache.Key (constructor error not checked?)")

// Get implements port.Cache.
func (c *Cache) Get(ctx context.Context, key aicache.Key) ([]byte, error) {
	if key.IsZero() {
		return nil, errZeroKey
	}
	value, err := c.client.Get(ctx, key.String()).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, port.ErrCacheMiss
	}
	if err != nil {
		return nil, fmt.Errorf("ai/adapter/redis: get: %w", err)
	}
	return value, nil
}

// Set implements port.Cache.
func (c *Cache) Set(ctx context.Context, key aicache.Key, value []byte, ttl time.Duration) error {
	if key.IsZero() {
		return errZeroKey
	}
	if err := c.client.Set(ctx, key.String(), value, ttl).Err(); err != nil {
		return fmt.Errorf("ai/adapter/redis: set: %w", err)
	}
	return nil
}

// Del implements port.Cache.
func (c *Cache) Del(ctx context.Context, key aicache.Key) error {
	if key.IsZero() {
		return errZeroKey
	}
	if err := c.client.Del(ctx, key.String()).Err(); err != nil {
		return fmt.Errorf("ai/adapter/redis: del: %w", err)
	}
	return nil
}

// Compile-time guard that *Cache implements port.Cache.
var _ port.Cache = (*Cache)(nil)
