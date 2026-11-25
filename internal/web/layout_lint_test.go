package web

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func checkTablesAreWrapped(templates map[string]string) []string {
	tableTag := regexp.MustCompile(`<table\b[^>]*class="[^"]*\btable\b[^"]*"[^>]*>`)
	wrapOpen := regexp.MustCompile(`<div\b[^>]*class="[^"]*\btable-wrap\b[^"]*"[^>]*>\s*$`)
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []string
	for _, name := range names {
		src := templates[name]
		for _, loc := range tableTag.FindAllStringIndex(src, -1) {
			prefix := src[:loc[0]]
			if !wrapOpen.MatchString(prefix) {
				line := strings.Count(prefix, "\n") + 1
				errs = append(errs, fmt.Sprintf("%s:%d renders <table class=\"table\"> without a <div class=\"table-wrap\"> wrapper; wrap it in <div class=\"table-wrap\"> so narrow screens scroll instead of clipping", name, line))
			}
		}
	}
	return errs
}

func TestTablesAreWrappedForHorizontalScroll(t *testing.T) {
	t.Parallel()
	for _, msg := range checkTablesAreWrapped(webAssetsByExt(t, ".html")) {
		t.Error(msg)
	}
	// Replay unwrapped table clipping (ab2d216 / account.html & usage.html).
	replay := `<div class="card"><table class="table"><tr><td>clipped</td></tr></table></div>`
	if got := checkTablesAreWrapped(map[string]string{"account.html": replay}); len(got) != 1 || !strings.Contains(got[0], "table-wrap") {
		t.Fatalf("replay unwrapped table: got %v, want 1 table-wrap finding", got)
	}
}

func checkFlexInputRowsWrap(sources map[string]string) []string {
	ruleRe := regexp.MustCompile(`([.#][a-zA-Z0-9_-]+(?:\s+input)?)\s*\{([^}]*)\}`)
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []string
	for _, name := range names {
		rules := map[string]string{}
		for _, m := range ruleRe.FindAllStringSubmatch(sources[name], -1) {
			sel := strings.Join(strings.Fields(m[1]), " ")
			rules[sel] += " " + m[2]
		}
		sels := make([]string, 0, len(rules))
		for sel := range rules {
			sels = append(sels, sel)
		}
		sort.Strings(sels)
		for _, sel := range sels {
			body := rules[sel]
			if !strings.Contains(body, "display: flex") && !strings.Contains(body, "display:flex") {
				continue
			}
			inputBody, hasInput := rules[sel+" input"]
			if !hasInput || !strings.Contains(inputBody, "flex:") {
				continue
			}
			if !strings.Contains(body, "flex-wrap: wrap") && !strings.Contains(body, "flex-wrap:wrap") {
				errs = append(errs, fmt.Sprintf("%s %s is a flex input row without flex-wrap: wrap; add flex-wrap: wrap so narrow screens do not clip controls", name, sel))
			}
			if !strings.Contains(inputBody, "min-width: 0") && !strings.Contains(inputBody, "min-width:0") {
				errs = append(errs, fmt.Sprintf("%s %s input uses flex without min-width: 0; add min-width: 0 so inputs shrink on phones", name, sel))
			}
		}
	}
	return errs
}

func TestFlexInputRowsWrapOnNarrowScreens(t *testing.T) {
	t.Parallel()
	for _, msg := range checkFlexInputRowsWrap(webAssetsByExt(t, ".css", ".html")) {
		t.Error(msg)
	}
	// Replay past bug ab2d216: email.html .owners flex row without flex-wrap: wrap or min-width: 0.
	replay := `<style>.owners { display: flex; gap: .5rem; } .owners input { flex: 1; }</style>`
	got := checkFlexInputRowsWrap(map[string]string{"email.html": replay})
	if len(got) != 2 {
		t.Fatalf("replay ab2d216 flex input row: got %v, want 2 findings (flex-wrap and min-width)", got)
	}
}
