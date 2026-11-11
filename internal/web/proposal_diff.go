package web

import (
	"html/template"
	"strings"
	"unicode"

	"github.com/zachlatta/intelligence/internal/workflow"
)

// Diff whitespace-preserving word tokens with the same bounded edit algorithm as
// lines. Escape each fragment before adding the only trusted markup: ins/del.
func proposalInlineDiff(before, after string) template.HTML {
	tokens := func(s string) []string {
		var out []string
		start := 0
		space := false
		for i, r := range s {
			next := unicode.IsSpace(r)
			if i > start && next != space {
				out = append(out, s[start:i])
				start = i
			}
			space = next
		}
		if start < len(s) {
			out = append(out, s[start:])
		}
		return out
	}
	a, b := tokens(before), tokens(after)
	var out strings.Builder
	kind := ""
	closeTag := func() {
		switch kind {
		case "added":
			out.WriteString("</ins>")
		case "removed":
			out.WriteString("</del>")
		}
	}
	for _, part := range workflow.ProposalTokenDiff(a, b) {
		if kind != part.Kind {
			closeTag()
			kind = part.Kind
			switch kind {
			case "added":
				out.WriteString("<ins>")
			case "removed":
				out.WriteString("<del>")
			}
		}
		out.WriteString(template.HTMLEscapeString(part.Text))
	}
	closeTag()
	return template.HTML(out.String()) // Only escaped input and fixed tags.
}

type proposalDiffLine = workflow.ProposalDiffLine

func proposalLineDiff(before, after string) []proposalDiffLine {
	return workflow.ProposalLineDiff(before, after)
}
