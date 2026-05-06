// notenant is a go vet tool that fails when code under internal/ calls
// *pgxpool.Pool.{Exec,Query,QueryRow,SendBatch,CopyFrom} directly. Wire it
// up in CI with:
//
//	go vet -vettool=$(which notenant) ./internal/...
//
// Build with: go build -o bin/notenant ./tools/lint/notenant/cmd/notenant
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/pericles-luz/crm/tools/lint/notenant"
)

func main() {
	singlechecker.Main(notenant.Analyzer)
}
