// Fixture: a `// nomathrand:ok` marker with no reason MUST NOT
// silence the rule. The lint exists to keep an audit trail; an
// unjustified suppression defeats that.
package emptyreasonpkg

// nomathrand:ok
import "math/rand" // want `file imports "math/rand" in \*gen.go scope`

func unsafeID() int { return rand.Int() }
