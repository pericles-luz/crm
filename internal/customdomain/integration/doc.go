// Package integration hosts integration tests for the SIN-62243 F45
// custom-domain stack against a real Postgres instance. Tests here are
// gated by the `integration` build tag so they stay out of the default
// `go test ./...` run, which is the fast unit suite.
//
// Run them with `make test-integration` (preferred — see Makefile target)
// or `go test -tags integration ./internal/customdomain/integration/...`.
//
// Test harness selection — same precedence as the webhook integration
// suite (TEST_POSTGRES_DSN env var > testcontainers-go postgres:16-alpine).
package integration
