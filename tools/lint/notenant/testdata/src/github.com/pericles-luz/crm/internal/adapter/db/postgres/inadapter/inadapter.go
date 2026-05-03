// Fixture for the analyzer's allowlist: code in the postgres adapter
// package itself MAY call *pgxpool.Pool methods directly because that's
// where the WithTenant / WithMasterOps wrappers live. No diagnostic
// expectations on this file.
package inadapter

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ok(ctx context.Context, p *pgxpool.Pool) {
	_, _ = p.Exec(ctx, "SELECT 1")
}
