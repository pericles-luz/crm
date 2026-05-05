// Fixture: an external test package for the postgres adapter. Go reports
// its import path as "<adapter>_test"; the analyzer's allowlist must strip
// that suffix so adapter tests can keep importing pgx like the adapter
// itself does.
package postgres_test

import (
	_ "database/sql"
	_ "github.com/jackc/pgx/v5"
)
