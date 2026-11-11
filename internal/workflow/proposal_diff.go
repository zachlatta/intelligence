package workflow

import "strings"

type ProposalDiffLine struct{ Kind, Text string }

// Myers' shortest edit script keeps unchanged lines between edits. Limit edit
// distance for unrelated, very large prompts so review cannot exhaust the server.
func ProposalLineDiff(before, after string) []ProposalDiffLine {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	return ProposalTokenDiff(a, b)
}

func ProposalTokenDiff(a, b []string) []ProposalDiffLine {
	v := map[int]int{1: 0}
	trace := []map[int]int{}
	for d := 0; d <= len(a)+len(b) && d <= 512; d++ {
		next := map[int]int{}
		for k := -d; k <= d; k += 2 {
			x := 0
			if k == -d || (k != d && v[k-1] < v[k+1]) {
				x = v[k+1]
			} else {
				x = v[k-1] + 1
			}
			y := x - k
			for x < len(a) && y < len(b) && a[x] == b[y] {
				x++
				y++
			}
			next[k] = x
			if x == len(a) && y == len(b) {
				out := []ProposalDiffLine{}
				for depth := d; depth > 0; depth-- {
					prev := trace[depth-1]
					diag := x - y
					pk := diag - 1
					if diag == -depth || (diag != depth && prev[diag-1] < prev[diag+1]) {
						pk = diag + 1
					}
					px := prev[pk]
					py := px - pk
					for x > px && y > py {
						x--
						y--
						out = append(out, ProposalDiffLine{"same", a[x]})
					}
					if x == px {
						y--
						out = append(out, ProposalDiffLine{"added", b[y]})
					} else {
						x--
						out = append(out, ProposalDiffLine{"removed", a[x]})
					}
				}
				for x > 0 && y > 0 {
					x--
					y--
					out = append(out, ProposalDiffLine{"same", a[x]})
				}
				for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
					out[i], out[j] = out[j], out[i]
				}
				return out
			}
		}
		trace = append(trace, next)
		v = next
	}
	out := make([]ProposalDiffLine, 0, len(a)+len(b))
	for _, s := range a {
		out = append(out, ProposalDiffLine{"removed", s})
	}
	for _, s := range b {
		out = append(out, ProposalDiffLine{"added", s})
	}
	return out
}
