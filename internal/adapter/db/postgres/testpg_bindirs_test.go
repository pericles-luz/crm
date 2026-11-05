package postgres_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

func TestPgBinDirs_NewestMajorFirst(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, withPgCtl bool) {
		dir := filepath.Join(root, name, "bin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if withPgCtl {
			if err := os.WriteFile(filepath.Join(dir, "pg_ctl"), nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("14", true)
	mk("18", true)
	mk("9", true)
	mk("17", false)     // no pg_ctl: skipped
	mk("current", true) // non-numeric: skipped

	got := testpg.PgBinDirs(root)
	want := []string{
		filepath.Join(root, "18", "bin"),
		filepath.Join(root, "14", "bin"),
		filepath.Join(root, "9", "bin"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PgBinDirs() = %v, want %v", got, want)
	}
}

func TestPgBinDirs_MissingRoot(t *testing.T) {
	if got := testpg.PgBinDirs(filepath.Join(t.TempDir(), "absent")); len(got) != 0 {
		t.Fatalf("PgBinDirs() = %v, want empty", got)
	}
}
