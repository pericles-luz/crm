// Package goredis is a stub of github.com/redis/go-redis/v9 used only by the
// aicache analyzer's testdata fixtures. It exposes just enough of the client
// surface for the analyzer's type-checks to fire.
package goredis

import (
	"context"
	"time"
)

// Client mimics the upper-level go-redis client.
type Client struct{}

// StringCmd, StatusCmd, IntCmd mimic the go-redis return cmd types.
type StringCmd struct{ val string }
type StatusCmd struct{ val string }
type IntCmd struct{ val int64 }

func (c *StringCmd) Bytes() ([]byte, error) { return []byte(c.val), nil }
func (c *StringCmd) Err() error             { return nil }
func (c *StatusCmd) Err() error             { return nil }
func (c *IntCmd) Err() error                { return nil }

// Get / Set / Del — the watched methods.
func (c *Client) Get(_ context.Context, _ string) *StringCmd {
	return &StringCmd{}
}
func (c *Client) Set(_ context.Context, _ string, _ any, _ time.Duration) *StatusCmd {
	return &StatusCmd{}
}
func (c *Client) Del(_ context.Context, _ ...string) *IntCmd {
	return &IntCmd{}
}
func (c *Client) Exists(_ context.Context, _ ...string) *IntCmd {
	return &IntCmd{}
}
func (c *Client) Incr(_ context.Context, _ string) *IntCmd {
	return &IntCmd{}
}
func (c *Client) Expire(_ context.Context, _ string, _ time.Duration) *StatusCmd {
	return &StatusCmd{}
}

// Nil is the sentinel returned on cache miss. The stub keeps it but the
// analyzer doesn't inspect it.
var Nil = errStub{}

type errStub struct{}

func (errStub) Error() string { return "redis: nil" }
