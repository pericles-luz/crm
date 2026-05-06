// Throwaway fixture for SIN-62302: verifies the govulncheck CI gate fails
// on a known-CALLED CVE (GO-2020-0017 / CVE-2020-26160 in dgrijalva/jwt-go).
// This package must NEVER be merged to main and must NEVER be wired into
// Makefile/Dockerfile/release pipelines. The throwaway branch
// verify/sin-62298-govulncheck-gate-fixture is deleted after the CI run
// is captured.
package main

import (
	"fmt"

	jwt "github.com/dgrijalva/jwt-go"
)

func main() {
	// GO-2020-0017 / CVE-2020-26160: MapClaims.VerifyAudience accepts an
	// audience supplied as a JSON array element matching ANY entry, when
	// the spec requires ALL configured audiences to match. Calling the
	// vulnerable symbol below makes govulncheck classify this fixture as
	// "Vulnerability is called", which is the gate condition under test.
	claims := jwt.MapClaims{
		"aud": []string{"a", "b"},
	}
	ok := claims.VerifyAudience("a", true)
	fmt.Printf("VerifyAudience=%v\n", ok)
}
