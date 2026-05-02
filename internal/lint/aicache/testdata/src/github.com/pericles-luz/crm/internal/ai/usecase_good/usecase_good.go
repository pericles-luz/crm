// Good case: a non-adapter package under internal/ai/ does NOT import redis.
// All cache reads/writes flow through port.Cache (omitted here for brevity).
package usecase_good

import (
	"github.com/pericles-luz/crm/internal/ai/cache"
)

func MakeKey(tenant, conv, msg string) string {
	k, _ := cache.TenantKey(tenant, conv, msg)
	return k.String()
}
