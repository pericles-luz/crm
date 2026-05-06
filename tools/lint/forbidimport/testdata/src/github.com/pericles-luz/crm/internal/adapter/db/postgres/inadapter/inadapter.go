// Fixture: the postgres adapter (and any sub-package under it) IS the
// seam between the domain and the SQL driver, so forbidden imports must
// not be flagged here.
package inadapter

import (
	_ "database/sql"
	_ "database/sql/driver"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/lib/pq"
)
