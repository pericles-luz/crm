// Fixture: a file ending in `gen.go` outside the webhook package must
// also be flagged (rule 2). This package path does NOT match the
// webhook substring; the suffix rule is what fires.
package genfilepkg

import (
	"math/rand" // want `file imports "math/rand" in \*gen.go scope`
)

func unsafeID() int { return rand.Int() }
