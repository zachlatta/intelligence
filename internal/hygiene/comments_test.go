package hygiene

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	restatingDocRe = regexp.MustCompile(
		`^//\s+([A-Za-z_][A-Za-z0-9_]*)\s+` +
			`(is|are|returns|gets|sets|reads|writes|creates|deletes|lists|checks|finds|loads|saves|` +
			`starts|stops|runs|handles|holds|keeps|stores|shows|formats|parses|builds|makes|computes|` +
			`counts|adds|removes|updates|closes|opens|sends|receives|reports|describes|represents|` +
			`defines|gives|takes|names|maps|converts|encodes|decodes|validates|verifies|executes|` +
			`renders|serves|fetches|posts|records|logs|prints|collects|tracks|watches|polls|` +
			`syncs|clears|resets|marks|links|matches|picks|selects|applies|merges|splits|joins|` +
			`trims|strips|wraps|unwraps|copies|clones|moves|renames|archives|cancels|resumes|retries|` +
			`waits|sleeps|ticks|wakes|notifies|alerts|claims|enrolls|signs|authenticates|authorizes|` +
			`reconciles|schedules|queues|dequeues|enqueues|launches|spawns|installs|uninstalls|` +
			`downloads|uploads|streams|dials|listens|accepts|connects|disconnects|pings|acks)\b(.*)$`,
	)
	constraintWordRe = regexp.MustCompile(
		`(?i)\b(because|why|never|must|cannot|can't|csrf|same-origin|rls|security|secret|` +
			`password|leak|race|lock|deadlock|idempoten|rollback|atomic|legacy|compat|bug|` +
			`rfc|utc|postgres|paseo|coolify|tailnet|public|private|audit|permission|bypass|` +
			`fallback|workaround|todo|fixme|warning|important|guard|replay|invariant|constraint|` +
			`contract|refuse|reject|forbid|prevent|avoid|ensure|preserve|unless|otherwise|instead|` +
			`rather|not |no |only |before |after |while |until |when |if |except|without|limit|bound|` +
			`max|min|at most|at least|default|nil|null|zero|empty|http|https|json|sql|css|html|utf|` +
			`ebpf|cgroup|oauth|jwt|mime|sha|argon|crockford|ascii|ansi|sse|mcp|pdw|gmail|assemblyai|` +
			`cswap|launchd|systemd|alpine|docker|git|node)\b`,
	)
)

func checkRestatingDocComments(filename, src string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return []string{fmt.Sprintf("%s: parse error: %v", filename, err)}
	}
	var errs []string
	checkDoc := func(doc *ast.CommentGroup, ident string) {
		if doc == nil || len(doc.List) != 1 || ident == "" {
			return
		}
		text := strings.TrimSpace(doc.List[0].Text)
		if strings.HasPrefix(text, "//go:") {
			return
		}
		m := restatingDocRe.FindStringSubmatch(text)
		if m == nil || m[1] != ident {
			return
		}
		if constraintWordRe.MatchString(text) {
			return
		}
		rest := strings.TrimSpace(m[3])
		if strings.ContainsAny(rest, "():\"`;/") || strings.Contains(rest, "->") || strings.Contains(rest, "http") {
			return
		}
		words := strings.Fields(strings.TrimSuffix(rest, "."))
		if len(words) <= 9 {
			pos := fset.Position(doc.Pos())
			errs = append(errs, fmt.Sprintf("%s:%d doc comment %q merely restates %s; delete it or state a non-obvious constraint/why", pos.Filename, pos.Line, text, ident))
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			checkDoc(d.Doc, d.Name.Name)
		case *ast.GenDecl:
			if len(d.Specs) != 1 {
				continue
			}
			switch s := d.Specs[0].(type) {
			case *ast.TypeSpec:
				checkDoc(d.Doc, s.Name.Name)
			case *ast.ValueSpec:
				if len(s.Names) == 1 {
					checkDoc(d.Doc, s.Names[0].Name)
				}
			}
		}
	}
	return errs
}

func TestNoRestatingDocComments(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for _, msg := range checkRestatingDocComments(rel, string(b)) {
				t.Error(msg)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoRestatingDocCommentsReplaysDeletedComments(t *testing.T) {
	replays := []struct {
		name string
		src  string
		want string
	}{
		{"HostMinute", "package fleet\n// HostMinute is a stored minute.\ntype HostMinute struct{}\n", "merely restates HostMinute"},
		{"OOMKill", "package fleet\n// OOMKill is a stored kill.\ntype OOMKill struct{}\n", "merely restates OOMKill"},
		{"HostResources", "package fleet\n// HostResources is every host's HostNow.\nfunc HostResources() {}\n", "merely restates HostResources"},
		{"Entry", "package whatsnew\n// Entry is one release note.\ntype Entry struct{}\n", "merely restates Entry"},
		{"Probe", "package probe\n// Probe is the loaded probe.\ntype Probe struct{}\n", "merely restates Probe"},
	}
	for _, tc := range replays {
		t.Run(tc.name, func(t *testing.T) {
			got := checkRestatingDocComments("replay.go", tc.src)
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("got %v, want 1 finding containing %q", got, tc.want)
			}
			t.Logf("caught deleted comment: %s", got[0])
		})
	}
	// Constraint/why comments must NOT be flagged.
	keep := "package web\n// write refuses cross-origin requests because another site cannot send JSON without CORS.\nfunc write() {}\n"
	if got := checkRestatingDocComments("auth.go", keep); len(got) != 0 {
		t.Fatalf("false positive on constraint comment: %v", got)
	}
}
