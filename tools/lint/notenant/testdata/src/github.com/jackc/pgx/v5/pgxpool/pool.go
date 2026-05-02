// Stub pgxpool package used only by analyzer tests so the import path
// "github.com/jackc/pgx/v5/pgxpool" resolves under GOPATH mode without
// pulling in the real (large) pgx module. The analyzer matches receiver
// types by package path, so the only thing that matters is that this file
// declares Pool and Conn with the pool methods we want to flag.
package pgxpool

import "context"

type Pool struct{}

type Conn struct{}

type Tx struct{}

type CommandTag struct{}

type Rows struct{}

type Row struct{}

type BatchResults struct{}

type Batch struct{}

func (p *Pool) Exec(ctx context.Context, sql string, args ...interface{}) (CommandTag, error) {
	return CommandTag{}, nil
}

func (p *Pool) Query(ctx context.Context, sql string, args ...interface{}) (*Rows, error) {
	return nil, nil
}

func (p *Pool) QueryRow(ctx context.Context, sql string, args ...interface{}) *Row {
	return nil
}

func (p *Pool) SendBatch(ctx context.Context, b *Batch) BatchResults {
	return BatchResults{}
}

func (p *Pool) CopyFrom(ctx context.Context, table []string, columns []string, rows interface{}) (int64, error) {
	return 0, nil
}

func (p *Pool) Begin(ctx context.Context) (*Tx, error) { return nil, nil }

func (c *Conn) Exec(ctx context.Context, sql string, args ...interface{}) (CommandTag, error) {
	return CommandTag{}, nil
}

func (c *Conn) Query(ctx context.Context, sql string, args ...interface{}) (*Rows, error) {
	return nil, nil
}

// Tx is here only so analyzer test fixtures can show that .Exec on a
// non-Pool/non-Conn type is NOT flagged.
func (t *Tx) Exec(ctx context.Context, sql string, args ...interface{}) (CommandTag, error) {
	return CommandTag{}, nil
}
