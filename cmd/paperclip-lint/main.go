// Command paperclip-lint bundles the project's custom go/analysis passes
// (currently nobodyreread + nosecrets, ADR 0075) into a single binary.
//
// Three usage shapes are supported:
//
//	paperclip-lint check ./...                     # explicit subcommand
//	paperclip-lint ./...                           # bare multichecker
//	go vet -vettool=$(which paperclip-lint) ./...  # vet integration
//
// All three are equivalent — `check` is just a no-op subcommand that
// developers can type without having to remember the multichecker shape.
// All standard multichecker flags (e.g. `-nobodyreread.foo`,
// `-nosecrets.webhook-substr=/internal/foo`) are forwarded.
package main

import (
	"os"

	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/pericles-luz/crm/tools/lint/nobodyreread"
	"github.com/pericles-luz/crm/tools/lint/nosecrets"
)

func main() {
	// Strip the optional `check` subcommand so multichecker.Main sees the
	// remaining args verbatim.
	if len(os.Args) >= 2 && os.Args[1] == "check" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	multichecker.Main(
		nobodyreread.Analyzer,
		nosecrets.Analyzer,
	)
}
