package web

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"text/template/parse"
)

func webAssetsByExt(t *testing.T, exts ...string) map[string]string {
	t.Helper()
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string)
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		for _, ext := range exts {
			if strings.HasSuffix(name, ext) {
				b, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				out[name] = string(b)
				break
			}
		}
	}
	return out
}

// TestEveryCSSVariableIsDefined: a var(--name) with no definition falls back to the property's
// initial value without a word, so a box once lost its rounded corners and shadow and a label
// turned monospace. Every variable a page or script reads is defined in ui.css or on that page.
func TestEveryCSSVariableIsDefined(t *testing.T) {
	sources := webAssetsByExt(t, ".css", ".html", ".js")
	defs := regexp.MustCompile(`(--[a-zA-Z0-9-]+)\s*:`)
	uses := regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9-]+)`)
	setProps := regexp.MustCompile(`setProperty\(\s*['"](--[a-zA-Z0-9-]+)`)
	shared := map[string]bool{}
	for _, m := range defs.FindAllStringSubmatch(string(uiCSS), -1) {
		shared[m[1]] = true
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		src := sources[name]
		local := map[string]bool{}
		for _, m := range defs.FindAllStringSubmatch(src, -1) {
			local[m[1]] = true
		}
		for _, m := range setProps.FindAllStringSubmatch(src, -1) {
			local[m[1]] = true
		}
		seen := map[string]bool{}
		for _, m := range uses.FindAllStringSubmatch(src, -1) {
			if v := m[1]; !shared[v] && !local[v] && !seen[v] {
				seen[v] = true
				t.Errorf("%s reads var(%s), which neither ui.css nor the file defines; define %s in internal/web/ui.css or locally in %s", name, v, v, name)
			}
		}
	}
}

// TestEverySharedCSSVariableIsRead: a ui.css token nothing reads is a palette entry that looks
// live but changes nothing (--accent-hover outlived its last reader when links and the primary
// button moved to --link).
func TestEverySharedCSSVariableIsRead(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range files {
		name := f.Name()
		if strings.HasSuffix(name, "_test.go") || !(strings.HasSuffix(name, ".css") || strings.HasSuffix(name, ".html") ||
			strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".go")) {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	read := map[string]bool{}
	for _, m := range regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9-]+)`).FindAllStringSubmatch(all.String(), -1) {
		read[m[1]] = true
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(--[a-zA-Z0-9-]+)\s*:`).FindAllStringSubmatch(string(uiCSS), -1) {
		if v := m[1]; !read[v] && !seen[v] {
			seen[v] = true
			t.Errorf("ui.css defines %s, which nothing reads", v)
		}
	}
}

// TestPagesDoNotRedefineSharedKeyframes: a page's own copy of a ui.css animation silently wins and
// drifts from the shared one.
func TestPagesDoNotRedefineSharedKeyframes(t *testing.T) {
	frames := regexp.MustCompile(`@keyframes\s+([a-zA-Z0-9_-]+)`)
	shared := map[string]bool{}
	for _, m := range frames.FindAllStringSubmatch(string(uiCSS), -1) {
		shared[m[1]] = true
	}
	for name, src := range webAssetsByExt(t, ".html") {
		for _, m := range frames.FindAllStringSubmatch(src, -1) {
			if shared[m[1]] {
				t.Errorf("%s redefines @keyframes %s from ui.css; remove the local copy and use ui.css", name, m[1])
			}
		}
	}
}

func checkNoNestedBodyTags(templates map[string]string) []string {
	bodyOpen := regexp.MustCompile(`(?i)<body\b`)
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []string
	for _, file := range names {
		tree := parse.New(file)
		tree.Mode = parse.SkipFuncCheck
		trees := make(map[string]*parse.Tree)
		if _, err := tree.Parse(templates[file], "{{", "}}", trees); err != nil {
			errs = append(errs, fmt.Sprintf("%s: parse template: %v", file, err))
			continue
		}
		var countBodies func(node parse.Node, stack map[string]bool) int
		countBodies = func(node parse.Node, stack map[string]bool) int {
			if node == nil {
				return 0
			}
			switch n := node.(type) {
			case *parse.ListNode:
				if n == nil {
					return 0
				}
				total := 0
				for _, child := range n.Nodes {
					total += countBodies(child, stack)
				}
				return total
			case *parse.TextNode:
				return len(bodyOpen.FindAll(n.Text, -1))
			case *parse.TemplateNode:
				sub := trees[n.Name]
				if sub == nil || stack[n.Name] {
					return 0
				}
				stack[n.Name] = true
				c := countBodies(sub.Root, stack)
				delete(stack, n.Name)
				return c
			case *parse.IfNode:
				return max(countBodies(n.List, stack), countBodies(n.ElseList, stack))
			case *parse.WithNode:
				return max(countBodies(n.List, stack), countBodies(n.ElseList, stack))
			case *parse.RangeNode:
				return max(countBodies(n.List, stack), countBodies(n.ElseList, stack))
			}
			return 0
		}
		tNames := make([]string, 0, len(trees))
		for tName := range trees {
			tNames = append(tNames, tName)
		}
		sort.Strings(tNames)
		for _, tName := range tNames {
			if n := countBodies(trees[tName].Root, map[string]bool{tName: true}); n > 1 {
				errs = append(errs, fmt.Sprintf("%s template %q emits %d <body> tags; remove the duplicate <body> tag so the page has one document body", file, tName, n))
			}
		}
	}
	return errs
}

func TestNoNestedBodyTagsInTemplates(t *testing.T) {
	for _, msg := range checkNoNestedBodyTags(webAssetsByExt(t, ".html")) {
		t.Error(msg)
	}
	replay := `{{define "head"}}<html><body>{{end}}{{define "agent"}}{{template "head"}}<body><main></main></body>{{end}}`
	if got := checkNoNestedBodyTags(map[string]string{"spawn.html": replay}); len(got) != 1 || !strings.Contains(got[0], `template "agent" emits 2 <body> tags`) {
		t.Fatalf("replay ab2d216: got %v, want 1 nested-body finding on agent", got)
	}
}

// TestPaletteIsNeutral: surfaces, lines, text and the emphasis colour (current tab, selection,
// focus, running pills) are warm or true neutrals in both schemes, never blue-tinted; blue is
// --link alone, which links and the primary button use.
func TestPaletteIsNeutral(t *testing.T) {
	css := string(uiCSS)
	dark := strings.Index(css, "@media (prefers-color-scheme: dark)")
	if dark < 0 {
		t.Fatal("ui.css has no dark scheme")
	}
	hex := regexp.MustCompile(`(--[a-z0-9-]+):\s*#([0-9a-fA-F]{6})\b`)
	neutral := map[string]bool{"--bg": true, "--bg-subtle": true, "--bg-muted": true, "--panel": true, "--line": true,
		"--line-strong": true, "--fg": true, "--fg-2": true, "--muted": true, "--faint": true, "--accent": true,
		"--accent-bg": true, "--accent-line": true}
	for scheme, block := range map[string]string{"light": css[:dark], "dark": css[dark:]} {
		seen := map[string]bool{}
		for _, m := range hex.FindAllStringSubmatch(block, -1) {
			if !neutral[m[1]] || seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			v, _ := strconv.ParseUint(m[2], 16, 32)
			r, b := int(v>>16), int(v&0xff)
			if b > r {
				t.Errorf("%s %s is #%s, which leans blue (blue %d > red %d)", scheme, m[1], m[2], b, r)
			}
		}
		for name := range neutral {
			if !seen[name] {
				t.Errorf("%s scheme does not set %s", scheme, name)
			}
		}
		if !strings.Contains(block, "--link:") {
			t.Errorf("%s scheme does not set --link", scheme)
		}
	}
	for _, want := range []string{"a { color: var(--link);", ".btn.primary { background: var(--link);"} {
		if !strings.Contains(css, want) {
			t.Errorf("ui.css lacks %q", want)
		}
	}
}
