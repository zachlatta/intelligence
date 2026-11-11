package web

import "testing"

func TestProposalHeadlineIsPlainText(t *testing.T) {
	for _, tc := range []struct{ summary, want string }{
		{"Improve **bold** and *italic* text", "Improve bold and italic text"},
		{"# Use `workflow_id` and [the guide](https://example.com/guide)", "Use workflow_id and the guide"},
		{"Use [**safe** guidance][ref]\n\n[ref]: https://example.com/guide", "Use safe guidance"},
		{"Keep `**literal**` and escaped \\*stars\\*", "Keep **literal** and escaped *stars*"},
		{"Names &amp; things\nSecond line", "Names & things"},
		{"Remove ~~obsolete~~ instructions", "Remove obsolete instructions"},
		{"\n> Read **this** first\n\nOther details", "Read this first"},
		{"", "Proposed update to Task"},
	} {
		p := proposalView{Summary: tc.summary, Operation: "update", Target: "Task"}
		if got := p.Headline(); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.summary, got, tc.want)
		}
	}
}
