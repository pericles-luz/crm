package openrouter_test

import (
	"testing"

	"github.com/pericles-luz/crm/internal/wallet/adapter/openrouter"
)

func TestUUIDs_NewID(t *testing.T) {
	g := openrouter.UUIDs{}
	a := g.NewID()
	b := g.NewID()
	if a == "" || b == "" {
		t.Fatalf("NewID returned empty")
	}
	if a == b {
		t.Fatalf("NewID returned duplicate id %q", a)
	}
	if len(a) < 8 {
		t.Fatalf("NewID returned suspiciously short id: %q", a)
	}
}
