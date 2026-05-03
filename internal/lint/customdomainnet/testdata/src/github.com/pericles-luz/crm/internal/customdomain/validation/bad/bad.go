// Bad fixture: a file under internal/customdomain/validation/* imports
// net/http. Rule 1 must fire on the import line.
package bad

import (
	"context"
	"net/http" // want `package github.com/pericles-luz/crm/internal/customdomain/validation/bad is under /internal/customdomain/validation but imports net/http; the validation use-case must stay net/http-free \(ADR 0079 §1\)`
)

// DoBadThing exists only so the import is "used" and the file compiles.
// The analyzer's job is to flag the import statement above; the body is
// irrelevant.
func DoBadThing(ctx context.Context) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://x/", nil)
}
