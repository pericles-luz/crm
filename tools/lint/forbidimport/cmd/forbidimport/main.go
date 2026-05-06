// forbidimport is a go vet tool that fails when any package outside
// internal/adapter/db/postgres imports database/sql, pgx, or lib/pq. Wire
// it up in CI with:
//
//	go vet -vettool=$(which forbidimport) ./internal/...
//
// Build with: go build -o bin/forbidimport ./tools/lint/forbidimport/cmd/forbidimport
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/pericles-luz/crm/tools/lint/forbidimport"
)

func main() {
	singlechecker.Main(forbidimport.Analyzer)
}
