package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryTestRunsInParallel: each test has its own database and server, so the package's tests
// run side by side. A test that cannot says why in its doc comment, starting a line with
// "Not parallel:".
func TestEveryTestRunsInParallel(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var serial []string
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			param := testingT(fn)
			if param == "" || callsParallel(fn, param) || exempt(fn) {
				continue
			}
			serial = append(serial, fset.Position(fn.Pos()).String()+" "+fn.Name.Name)
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

func exempt(fn *ast.FuncDecl) bool {
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
