// Fixture: every forbidden import here should be flagged. Diagnostic
// expectations use the analysistest convention.
package badpkg

import (
	_ "database/sql"            // want `forbidden import "database/sql" outside the postgres adapter packages`
	_ "database/sql/driver"     // want `forbidden import "database/sql/driver" outside the postgres adapter packages`
	_ "github.com/jackc/pgx/v5" // want `forbidden import "github.com/jackc/pgx/v5" outside the postgres adapter packages`
	_ "github.com/lib/pq"       // want `forbidden import "github.com/lib/pq" outside the postgres adapter packages`
)
