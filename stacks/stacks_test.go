package stacks

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestExtract(t *testing.T) {
	if !slices.Contains(Names(), "next-drizzle") {
		t.Fatalf("shipped stacks: %v", Names())
	}
	dir, err := Extract("next-drizzle", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "setup.sh"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("setup.sh not extracted as a script: %v %v", st, err)
	}
	if _, err := Extract("../etc", t.TempDir()); err == nil {
		t.Error("a name outside the shipped stacks was extracted")
	}
}
