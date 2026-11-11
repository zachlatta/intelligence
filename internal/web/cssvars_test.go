package web

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryCSSVariableIsDefined: a var(--name) with no definition falls back to the property's
// initial value without a word, so a box once lost its rounded corners and shadow and a label
// turned monospace. Every variable a page or script reads is defined in ui.css or on that page.
func TestEveryCSSVariableIsDefined(t *testing.T) {
	sources := map[string]string{
		"ui.css": string(uiCSS), "layout": layoutHTML, "index": indexHTML, "resources": resourcesHTML,
		"schedules": schedulesHTML, "spawn": spawnHTML, "account": accountHTML, "usage": usageHTML, "plan": planHTML,
		"transcript": transcriptHTML, "blocks": blocksHTML, "workflows": workflowsHTML, "hostresources": hostResourcesHTML,
		"email": emailHTML, "dictation.js": string(dictationJS), "submitkeys.js": string(submitKeysJS),
		"attachments.js": string(attachmentsJS),
	}
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
				t.Errorf("%s reads var(%s), which neither ui.css nor the page defines", name, v)
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
	for name, src := range map[string]string{
		"layout": layoutHTML, "index": indexHTML, "resources": resourcesHTML, "schedules": schedulesHTML,
		"spawn": spawnHTML, "account": accountHTML, "usage": usageHTML, "plan": planHTML, "transcript": transcriptHTML,
		"blocks": blocksHTML, "workflows": workflowsHTML, "hostresources": hostResourcesHTML,
	} {
		for _, m := range frames.FindAllStringSubmatch(src, -1) {
			if shared[m[1]] {
				t.Errorf("%s redefines @keyframes %s from ui.css", name, m[1])
			}
		}
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
