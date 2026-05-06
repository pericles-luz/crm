// Command customdomainnet runs the SIN-62242 net/http guard analyzer as a
// standalone vet tool.
//
// Usage in CI:
//
//	go vet -vettool=$(which customdomainnet) ./internal/customdomain/...
//
// The analyzer enforces ADR 0079 §1: no file under
// internal/customdomain/validation/* may import net/http.
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/pericles-luz/crm/internal/lint/customdomainnet"
)

func main() { singlechecker.Main(customdomainnet.Analyzer) }
