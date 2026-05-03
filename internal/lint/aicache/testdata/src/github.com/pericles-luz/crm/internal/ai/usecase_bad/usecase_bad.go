// Bad case: a non-adapter package under internal/ai/ imports the redis
// client directly. Rule 1 must fire on the import line.
package usecase_bad

import (
	"context"

	goredis "github.com/redis/go-redis/v9" // want `package github.com/pericles-luz/crm/internal/ai/usecase_bad is under /internal/ai/ but imports redis directly`
)

func DoBadThing(ctx context.Context, c *goredis.Client) {
	_ = c.Get(ctx, "tenant:acme:ai:summary:c:m")
}
