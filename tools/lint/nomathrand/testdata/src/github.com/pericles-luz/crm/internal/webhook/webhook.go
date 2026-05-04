// Fixture: a file under /internal/webhook/ that imports math/rand
// must be flagged (rule 1). The import line carries the `want`
// directive analysistest looks for.
package webhook

import (
	"math/rand" // want `file imports "math/rand" in webhook scope`
)

func RandomDigit() int {
	return rand.Intn(10)
}
