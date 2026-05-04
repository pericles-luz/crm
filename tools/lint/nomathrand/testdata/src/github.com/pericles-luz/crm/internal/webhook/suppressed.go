// Fixture: a `// nomathrand:ok` marker on the same line as the
// import (or the line above) silences the rule. The analyzer must NOT
// emit a diagnostic on this file even though the package path matches
// the webhook substring.
package webhook

// nomathrand:ok intentional — fixture for suppression test
import _ "math/rand/v2"
