// Package integration hosts integration tests for the SIN-62234 webhook
// stack against a real Postgres instance (no database/sql mocks). Tests
// here are gated by the `integration` build tag so they stay out of the
// default `go test ./...` run, which is the fast unit suite.
//
// Run them with `make test-integration` (preferred) or
// `go test -tags integration ./internal/webhook/integration/...`.
//
// Test harness selection — the first option that matches is used:
//
//   1. TEST_POSTGRES_DSN env var. The caller (CI side-car, local
//      developer with a Postgres install) provides a clean database
//      DSN; the harness applies migrations 0075a..0075d into it.
//
//   2. testcontainers-go postgres:16-alpine. The default for ad-hoc
//      runs on machines that have Docker available. Started once per
//      `go test` process via TestMain.
//
// If neither is reachable, the harness fails the test with a clear
// diagnostic so we never silently skip integration coverage.
package integration
