// Fixture: callers that already go through pgx.Tx (which is what
// WithTenant hands them) should not be flagged.
package goodpkg

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// usingTx represents what callers do inside a WithTenant fn: they get a
// pgx.Tx. tx.Exec is allowed.
func usingTx(ctx context.Context, tx *pgxpool.Tx) {
	_, _ = tx.Exec(ctx, "SELECT 1")
}

// usingNonPGXType: Exec on something that isn't a pgx pool/conn must not
// be flagged.
type otherDB struct{}

func (o *otherDB) Exec(ctx context.Context, sql string) (int, error) { return 0, nil }

func usingOther(ctx context.Context, o *otherDB) {
	_, _ = o.Exec(ctx, "SELECT 1")
}

var _ = pgxpool.CommandTag{} // keep the import live
