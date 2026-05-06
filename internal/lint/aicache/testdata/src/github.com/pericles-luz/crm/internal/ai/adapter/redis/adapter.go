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

// helperKey is a free function (not a method on cache.Key) that fabricates a
// raw key. The analyzer must reject it because the call expression's function
// is an Ident, not a SelectorExpr — exercising the non-selector early-return
// in isCacheKeyStringCall.
func helperKey() string { return "tenant:smuggled:ai:summary:c:m" }

func BadFreeFuncKey(ctx context.Context, c *goredis.Client) {
	_ = c.Get(ctx, helperKey()) // want `redis.Get key argument must be cache.Key.String\(\)`
}

// envelope wraps a non-Key value but exposes a String() method. Calling
// .String() on it must still be flagged because the receiver type is not
// defined in the cache package — exercising the named-type-but-wrong-package
// branch of isCacheKeyStringCall.
type envelope struct{ s string }

func (e envelope) String() string { return e.s }

func BadNonCacheKeyStringCall(ctx context.Context, c *goredis.Client) {
	e := envelope{s: "tenant:foo:ai:summary:c:m"}
	_ = c.Get(ctx, e.String()) // want `redis.Get key argument must be cache.Key.String\(\)`
}
