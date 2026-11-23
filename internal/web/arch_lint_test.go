package web

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func checkWriteRoutesAreUnderAPIOrMCP(filename, src string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return []string{fmt.Sprintf("%s: parse error: %v", filename, err)}
	}
	var errs []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		pattern := leadingStringLiteral(call.Args[0])
		if pattern == "" {
			return true
		}
		method, routePath, hasSpace := strings.Cut(pattern, " ")
		if !hasSpace {
			return true
		}
		switch method {
		case "POST", "PUT", "DELETE", "PATCH":
			if !strings.HasPrefix(routePath, "/api/") && !strings.HasPrefix(routePath, "/mcp") && routePath != "" {
				pos := fset.Position(call.Pos())
				errs = append(errs, fmt.Sprintf("%s:%d registers write route %q outside /api/; move mutations under /api/ and call them with JSON from the browser", pos.Filename, pos.Line, pattern))
			}
		}
		return true
	})
	return errs
}

func leadingStringLiteral(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err == nil {
				return s
			}
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return leadingStringLiteral(v.X) + leadingStringLiteral(v.Y)
		}
	}
	return ""
}

func TestWriteRoutesAreUnderAPIOrMCP(t *testing.T) {
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range checkWriteRoutesAreUnderAPIOrMCP("server.go", string(b)) {
		t.Error(msg)
	}
	replay := `package web
func reg(mux any) { mux.HandleFunc("POST /workflows/edit", nil) }`
	if got := checkWriteRoutesAreUnderAPIOrMCP("server.go", replay); len(got) != 1 || !strings.Contains(got[0], "outside /api/") {
		t.Fatalf("replay non-API write route: got %v, want 1 finding", got)
	}
}

func checkHandlersUseSharedRequireJSON(filename, src string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return []string{fmt.Sprintf("%s: parse error: %v", filename, err)}
	}
	var errs []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name == "requireJSON" {
			continue
		}
		hasContentTypeRead := false
		hasJSONCompare := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Get" && len(node.Args) == 1 {
					if lit, ok := node.Args[0].(*ast.BasicLit); ok && lit.Value == `"Content-Type"` {
						hasContentTypeRead = true
					}
				}
			case *ast.BinaryExpr:
				if node.Op == token.EQL || node.Op == token.NEQ {
					for _, side := range []ast.Expr{node.X, node.Y} {
						if lit, ok := side.(*ast.BasicLit); ok && lit.Value == `"application/json"` {
							hasJSONCompare = true
						}
					}
				}
			}
			return true
		})
		if hasContentTypeRead && hasJSONCompare {
			pos := fset.Position(fn.Pos())
			errs = append(errs, fmt.Sprintf("%s:%d %s hand-rolls Content-Type application/json validation; call requireJSON(w, r) instead", pos.Filename, pos.Line, fn.Name.Name))
		}
	}
	return errs
}

func TestHandlersUseSharedRequireJSON(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range checkHandlersUseSharedRequireJSON(name, string(b)) {
			t.Error(msg)
		}
	}
	// Replay past bug f89f3d5: mcp.go hand-rolling Content-Type JSON check instead of requireJSON.
	replay := `package web
import ("mime"; "net/http")
func mcp(w http.ResponseWriter, r *http.Request) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "bad", 415)
	}
}`
	if got := checkHandlersUseSharedRequireJSON("mcp.go", replay); len(got) != 1 || !strings.Contains(got[0], "requireJSON(w, r)") {
		t.Fatalf("replay f89f3d5: got %v, want 1 requireJSON finding", got)
	}
}

func checkProposalHelpCoversType(help string, typ reflect.Type) []string {
	var errs []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}
		if !strings.Contains(help, name) {
			errs = append(errs, fmt.Sprintf("proposalHelp in internal/web/mcp_proposals.go does not document %s.%s (json %q); add %q to proposalHelp", typ.Name(), f.Name, name, name))
		}
	}
	return errs
}

func TestMCPProposalHelpCoversWorkflowFields(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[workflow.Definition](),
		reflect.TypeFor[workflow.Orchestrator](),
		reflect.TypeFor[workflow.Step](),
	} {
		for _, msg := range checkProposalHelpCoversType(proposalHelp, typ) {
			t.Error(msg)
		}
	}
	// Replay past bug 98a1fd8: serviceTier omitted from proposalHelp.
	stripped := strings.ReplaceAll(proposalHelp, "serviceTier", "")
	if got := checkProposalHelpCoversType(stripped, reflect.TypeFor[workflow.Orchestrator]()); len(got) != 1 || !strings.Contains(got[0], "serviceTier") {
		t.Fatalf("replay 98a1fd8: got %v, want 1 missing serviceTier finding", got)
	}
}
