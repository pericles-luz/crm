// Fixture: every forbidden import here should be flagged. Diagnostic
// expectations use the analysistest convention.
package badpkg

import (
	_ "database/sql"            // want `forbidden import "database/sql" outside internal/adapter/db/postgres`
	_ "database/sql/driver"     // want `forbidden import "database/sql/driver" outside internal/adapter/db/postgres`
	_ "github.com/jackc/pgx/v5" // want `forbidden import "github.com/jackc/pgx/v5" outside internal/adapter/db/postgres`
	_ "github.com/lib/pq"       // want `forbidden import "github.com/lib/pq" outside internal/adapter/db/postgres`
)
