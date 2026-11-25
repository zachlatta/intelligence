package dbtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// databasePackages are the directories whose tests use dbtest, and of those, the ones whose
// TestMain clones a template.
func databasePackages(t *testing.T) (uses, clones map[string]bool) {
	t.Helper()
	root := filepath.Join("..", "..")
	uses, clones = map[string]bool{}, map[string]bool{}
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
	return uses, clones
}

// TestEveryDatabasePackageClonesATemplate: a package whose tests make databases migrates once, in
// TestMain, and clones that template for each test, instead of running every migration per test.
func TestEveryDatabasePackageClonesATemplate(t *testing.T) {
	t.Parallel()
	uses, clones := databasePackages(t)
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

// TestEveryDatabasePackageTestRunsInParallel: each test gets its own database, so the tests of a
// package that uses dbtest run side by side. A test that cannot says why in its doc comment, on a
// line starting "Not parallel:".
func TestEveryDatabasePackageTestRunsInParallel(t *testing.T) {
	t.Parallel()
	uses, _ := databasePackages(t)
	fset := token.NewFileSet()
	var serial []string
	for dir := range uses {
		files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range files {
			f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
					continue
				}
				param := testingT(fn)
				if param == "" || callsParallel(fn, param) || notParallel(fn) {
					continue
				}
				serial = append(serial, fset.Position(fn.Pos()).String()+" "+fn.Name.Name)
			}
		}
	}
	sort.Strings(serial)
	if len(serial) > 0 {
		t.Errorf("%d tests do not start with t.Parallel():\n%s", len(serial), strings.Join(serial, "\n"))
	}
}

// testingT is the name of fn's *testing.T parameter, if fn is a test.
func testingT(fn *ast.FuncDecl) string {
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) != 1 {
		return ""
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return ""
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return ""
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "testing" {
		return ""
	}
	return params[0].Names[0].Name
}

// callsParallel reports whether fn's first statement is t.Parallel().
func callsParallel(fn *ast.FuncDecl, t string) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	stmt, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Parallel" {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == t
}

func notParallel(fn *ast.FuncDecl) bool {
	if fn.Doc == nil {
		return false
	}
	for _, line := range strings.Split(fn.Doc.Text(), "\n") {
		if strings.HasPrefix(line, "Not parallel:") {
			return true
		}
	}
	return false
}

// TestParallelCheckReadsTestsAndOptOuts: the check above accepts a test whose first statement is
// t.Parallel() on its own *testing.T, or whose doc comment has a "Not parallel:" line, and no other.
func TestParallelCheckReadsTestsAndOptOuts(t *testing.T) {
	t.Parallel()
	src := `package p

import "testing"

func TestFirst(t *testing.T) { t.Parallel() }

func TestOtherName(tt *testing.T) { tt.Parallel() }

func TestLater(t *testing.T) { _ = 1; t.Parallel() }

func TestSerial(t *testing.T) {}

// TestOptedOut sets the environment.
//
// Not parallel: it calls t.Setenv.
func TestOptedOut(t *testing.T) {}

// TestMentionsIt says Not parallel: only mid-line.
func TestMentionsIt(t *testing.T) {}

func TestMain(m *testing.M) {}
`
	f, err := parser.ParseFile(token.NewFileSet(), "p_test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"TestFirst": true, "TestOtherName": true, "TestLater": false, "TestSerial": false, "TestOptedOut": true, "TestMentionsIt": false, "TestMain": true}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		param := testingT(fn)
		passes := param == "" || callsParallel(fn, param) || notParallel(fn)
		if passes != want[fn.Name.Name] {
			t.Errorf("%s passes = %v, want %v", fn.Name.Name, passes, want[fn.Name.Name])
		}
	}
}
