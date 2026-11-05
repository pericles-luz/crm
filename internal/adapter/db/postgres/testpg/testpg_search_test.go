package testpg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPgBinSearchPaths_NewestMajorFirst(t *testing.T) {
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

	old := pgLibRoot
	pgLibRoot = root
	t.Cleanup(func() { pgLibRoot = old })

	got := pgBinSearchPaths()
	want := []string{
		filepath.Join(root, "18", "bin"),
		filepath.Join(root, "14", "bin"),
		filepath.Join(root, "9", "bin"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pgBinSearchPaths() = %v, want %v", got, want)
	}
}

func TestPgBinSearchPaths_MissingRoot(t *testing.T) {
	old := pgLibRoot
	pgLibRoot = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { pgLibRoot = old })

	if got := pgBinSearchPaths(); len(got) != 0 {
		t.Fatalf("pgBinSearchPaths() = %v, want empty", got)
	}
}
