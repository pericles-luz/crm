// Fixture: forbidden imports silenced by an annotated // forbidimport:ok
// marker. The analyzer must stay silent so reviewers see only the marker
// in the diff.
package overridepkg

import (
	// forbidimport:ok perf-test fixture only; never linked into prod
	_ "database/sql"

	_ "database/sql/driver" // forbidimport:ok script-only fixture, never linked into prod
)
