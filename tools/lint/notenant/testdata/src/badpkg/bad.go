// Fixture: every direct *pgxpool.Pool / *pgxpool.Conn data-method call here
// should be flagged by the notenant analyzer. Diagnostic-expectation
// comments use the analysistest convention (see analyzer_test.go).
package badpkg

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func usePoolExec(ctx context.Context, p *pgxpool.Pool) {
	_, _ = p.Exec(ctx, "DELETE FROM customers") // want `direct \*pgxpool.Pool.Exec bypasses tenant scoping`
}

func usePoolQuery(ctx context.Context, p *pgxpool.Pool) {
	_, _ = p.Query(ctx, "SELECT 1") // want `direct \*pgxpool.Pool.Query bypasses tenant scoping`
}

func usePoolQueryRow(ctx context.Context, p *pgxpool.Pool) {
	_ = p.QueryRow(ctx, "SELECT 1") // want `direct \*pgxpool.Pool.QueryRow bypasses tenant scoping`
}

func usePoolSendBatch(ctx context.Context, p *pgxpool.Pool, b *pgxpool.Batch) {
	_ = p.SendBatch(ctx, b) // want `direct \*pgxpool.Pool.SendBatch bypasses tenant scoping`
}

func usePoolCopyFrom(ctx context.Context, p *pgxpool.Pool) {
	_, _ = p.CopyFrom(ctx, []string{"x"}, []string{"y"}, nil) // want `direct \*pgxpool.Pool.CopyFrom bypasses tenant scoping`
}

func useConnExec(ctx context.Context, c *pgxpool.Conn) {
	_, _ = c.Exec(ctx, "DELETE FROM customers") // want `direct \*pgxpool.Conn.Exec bypasses tenant scoping`
}

// Begin / BeginTx remain allowed (the helper itself uses them); no want.
func usePoolBeginIsAllowed(ctx context.Context, p *pgxpool.Pool) {
	_, _ = p.Begin(ctx)
}
