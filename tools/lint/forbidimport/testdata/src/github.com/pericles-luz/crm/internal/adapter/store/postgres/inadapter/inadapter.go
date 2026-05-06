// Fixture: the store/postgres adapter is the second allowlisted postgres
// adapter sub-package — store implementations consume the pool seam from
// db/postgres and expose clean domain ports upward, so forbidden imports
// must not be flagged here either.
package inadapter

import (
	_ "database/sql"
	_ "database/sql/driver"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/lib/pq"
)
