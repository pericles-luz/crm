// Command aicache runs the SIN-62236 cache-key analyzer as a standalone vet
// tool.
//
// Usage in CI:
//
//	go vet -vettool=$(which aicache) ./internal/ai/...
//
// The analyzer enforces ADR 0077 §3.4 on every package whose import path
// contains the configured AI substring (default "/internal/ai/").
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/pericles-luz/crm/internal/lint/aicache"
)

func main() { singlechecker.Main(aicache.Analyzer) }
