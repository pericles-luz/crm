// Adapter package: legitimate redis user. Good calls pass cache.Key.String();
// bad calls pass raw strings or cache.TenantKey results without going through
// the opaque Key. The aicache analyzer must flag the bad calls and leave the
// good ones alone.
package redis

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/ai/cache"
)

func GoodGet(ctx context.Context, c *goredis.Client, k cache.Key) {
	_ = c.Get(ctx, k.String())
}

func GoodSet(ctx context.Context, c *goredis.Client, k cache.Key) {
	_ = c.Set(ctx, k.String(), []byte("v"), time.Minute)
}

func GoodDel(ctx context.Context, c *goredis.Client, k cache.Key) {
	_ = c.Del(ctx, k.String())
}

func BadRawString(ctx context.Context, c *goredis.Client) {
	_ = c.Get(ctx, "tenant:acme:ai:summary:c:m") // want `redis.Get key argument must be cache.Key.String\(\)`
}

func BadFmtString(ctx context.Context, c *goredis.Client, tenant string) {
	key := "tenant:" + tenant + ":ai:summary:c:m"
	_ = c.Get(ctx, key) // want `redis.Get key argument must be cache.Key.String\(\)`
}

func BadSetWithRawString(ctx context.Context, c *goredis.Client) {
	_ = c.Set(ctx, "tenant:acme:ai:summary:c:m", []byte("v"), time.Minute) // want `redis.Set key argument must be cache.Key.String\(\)`
}

func BadDelWithRawString(ctx context.Context, c *goredis.Client) {
	_ = c.Del(ctx, "tenant:acme:ai:summary:c:m") // want `redis.Del key argument must be cache.Key.String\(\)`
}
