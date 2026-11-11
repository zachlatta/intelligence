package web

import "strings"

type proposalDiffLine struct{ Kind, Text string }

// Myers' shortest edit script keeps unchanged lines between edits. Limit edit
// distance for unrelated, very large prompts so review cannot exhaust the server.
func proposalLineDiff(before, after string) []proposalDiffLine {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
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
				out := []proposalDiffLine{}
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
						out = append(out, proposalDiffLine{"same", a[x]})
					}
					if x == px {
						y--
						out = append(out, proposalDiffLine{"added", b[y]})
					} else {
						x--
						out = append(out, proposalDiffLine{"removed", a[x]})
					}
				}
				for x > 0 && y > 0 {
					x--
					y--
					out = append(out, proposalDiffLine{"same", a[x]})
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
	out := make([]proposalDiffLine, 0, len(a)+len(b))
	for _, s := range a {
		out = append(out, proposalDiffLine{"removed", s})
	}
	for _, s := range b {
		out = append(out, proposalDiffLine{"added", s})
	}
	return out
}
