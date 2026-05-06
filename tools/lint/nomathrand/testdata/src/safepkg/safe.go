// Fixture: a package with no webhook substring in its path AND a
// filename that does NOT end in `gen.go` is OUT of scope. The
// math/rand import is allowed here — non-webhook randomness (e.g.
// load test jitter) does not need CSPRNG.
package safepkg

import "math/rand"

func Jitter() int { return rand.Intn(100) }
