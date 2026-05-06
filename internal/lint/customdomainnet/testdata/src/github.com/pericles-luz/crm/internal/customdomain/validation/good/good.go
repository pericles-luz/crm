// Good fixture: a file under internal/customdomain/validation/* that
// imports only allowed stdlib packages. The analyzer must NOT report any
// diagnostic.
package good

import (
	"context"
	"net/netip"
	"strings"
)

// DoGoodThing keeps the imports honest.
func DoGoodThing(_ context.Context, host string) netip.Addr {
	host = strings.TrimSpace(host)
	addr, _ := netip.ParseAddr(host)
	return addr
}
