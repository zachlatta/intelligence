package dbtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryDatabasePackageClonesATemplate: a package whose tests make databases migrates once, in
// TestMain, and clones that template for each test, instead of running every migration per test.
func TestEveryDatabasePackageClonesATemplate(t *testing.T) {
	root := filepath.Join("..", "..")
	uses, clones := map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && path != root && strings.HasPrefix(e.Name(), ".") {
			return filepath.SkipDir
		}
		if e.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		if dir == "." {
			return nil // this package
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), `"github.com/zachlatta/intelligence/internal/dbtest"`) {
			uses[dir] = true
		}
		if strings.Contains(string(src), "dbtest.RunWithTemplate(m)") {
			clones[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) == 0 {
		t.Fatal("found no package that uses dbtest: the walk is broken")
	}
	var missing []string
	for dir := range uses {
		if !clones[dir] {
			missing = append(missing, dir)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these packages use dbtest without a TestMain that calls os.Exit(dbtest.RunWithTemplate(m)): %v", missing)
	}
}
