// Fixture: a bare // forbidimport:ok marker (no justification) does NOT
// silence the rule. Reviewers should see a real reason in the diff.
package bareoverride

import (
	// forbidimport:ok
	_ "database/sql" // want `forbidden import "database/sql" outside internal/adapter/db/postgres`
)
