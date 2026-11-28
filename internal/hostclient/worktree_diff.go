package hostclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	maxWorktreeDiffFiles = 200
	maxFileDiffBytes     = 32 * 1024
	maxTotalDiffBytes    = 256 * 1024
	emptyGitTree         = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// WorktreeDiffResult is the uncommitted-changes snapshot of a workspace's git checkout.
type WorktreeDiffResult struct {
	Clean     bool               `json:"clean"`
	Files     []WorktreeDiffFile `json:"files,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// WorktreeDiffFile is one uncommitted file in a worktree diff.
type WorktreeDiffFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"` // "M", "A", "D", "R", "?"
	Binary    bool   `json:"binary,omitempty"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
	Diff      string `json:"diff,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type wtStatusEntry struct {
	path   string
	status string
}

// inspectWorktreeDiff inspects dir with git for uncommitted (staged, unstaged, and untracked)
// changes and returns bounded unified diffs suitable for browsing in the UI.
func inspectWorktreeDiff(ctx context.Context, dir string) WorktreeDiffResult {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return WorktreeDiffResult{Error: "workspace directory is not known on this host"}
	}
	if _, err := os.Stat(dir); err != nil {
		return WorktreeDiffResult{Error: "workspace directory is missing or unreadable"}
	}

	statusOut, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain=v1", "-z", "--untracked-files=all").Output()
	if err != nil {
		return WorktreeDiffResult{Error: "could not read git status for this worktree"}
	}
	entries := parsePorcelainZ(statusOut)
	if len(entries) == 0 {
		return WorktreeDiffResult{Clean: true}
	}

	res := WorktreeDiffResult{Clean: false}
	if len(entries) > maxWorktreeDiffFiles {
		entries = entries[:maxWorktreeDiffFiles]
		res.Truncated = true
	}

	// Determine diff base: HEAD if at least one commit exists, else git's empty tree hash.
	base := "HEAD"
	if exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", "HEAD").Run() != nil {
		base = emptyGitTree
	}

	totalBytes := 0
	res.Files = make([]WorktreeDiffFile, 0, len(entries))
	for _, e := range entries {
		f := WorktreeDiffFile{Path: e.path, Status: e.status}
		remaining := maxTotalDiffBytes - totalBytes
		if remaining <= 0 {
			f.Truncated = true
			res.Truncated = true
			res.Files = append(res.Files, f)
			continue
		}
		limit := min(maxFileDiffBytes, remaining)

		if e.status == "?" {
			fillUntrackedDiff(dir, &f, limit)
		} else {
			fillTrackedDiff(ctx, dir, base, &f, limit)
		}
		totalBytes += len(f.Diff)
		if f.Truncated {
			res.Truncated = true
		}
		res.Files = append(res.Files, f)
	}
	return res
}

func parsePorcelainZ(raw []byte) []wtStatusEntry {
	tokens := bytes.Split(raw, []byte{0})
	var out []wtStatusEntry
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 {
			continue
		}
		x, y := tok[0], tok[1]
		p := string(tok[3:])
		// Renames and copies emit a second NUL-terminated path for the original file.
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++
		}
		var st string
		switch {
		case x == '?' && y == '?':
			st = "?"
		case x == 'D' || y == 'D':
			st = "D"
		case x == 'R' || y == 'R':
			st = "R"
		case x == 'A' || y == 'A':
			st = "A"
		default:
			st = "M"
		}
		out = append(out, wtStatusEntry{path: p, status: st})
	}
	return out
}

func fillUntrackedDiff(dir string, f *WorktreeDiffFile, limit int) {
	data, ok := readUntracked(filepath.Join(dir, filepath.FromSlash(f.Path)), limit)
	if !ok {
		return
	}
	probeLen := min(len(data), 8192)
	if bytes.IndexByte(data[:probeLen], 0) >= 0 {
		f.Binary = true
		return
	}
	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}
	text := string(data)
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	f.Additions = len(lines)

	var b strings.Builder
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", f.Path, len(lines))
	for _, line := range lines {
		if b.Len()+len(line)+2 > limit {
			truncated = true
			break
		}
		b.WriteByte('+')
		b.WriteString(line)
		b.WriteByte('\n')
	}
	f.Diff = strings.TrimRight(b.String(), "\n")
	f.Truncated = truncated
}

// readUntracked reads up to limit+1 bytes of an untracked file, so the caller knows when it is
// larger than the cap. A symlink reads as its target, as git records it, so the diff never
// carries the contents of a file it points to (which may be outside the worktree). Anything else
// that is not a regular file (a FIFO, a socket) is not opened.
func readUntracked(path string, limit int) ([]byte, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return []byte(target), err == nil
	}
	if !info.Mode().IsRegular() {
		return nil, false
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer fh.Close()
	if opened, err := fh.Stat(); err != nil || !os.SameFile(info, opened) {
		return nil, false // replaced between Lstat and Open
	}
	data, err := io.ReadAll(io.LimitReader(fh, int64(limit)+1))
	return data, err == nil
}

func fillTrackedDiff(ctx context.Context, dir, base string, f *WorktreeDiffFile, limit int) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", base, "--", f.Path).Output()
	if err != nil || len(out) == 0 {
		return
	}
	if isBinaryGitDiff(out) {
		f.Binary = true
		return
	}
	// Count additions/deletions across the full diff output before truncating display text.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			f.Additions++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			f.Deletions++
		}
	}
	if len(out) > limit {
		f.Diff = trimUTF8Bytes(out[:limit])
		f.Truncated = true
	} else {
		f.Diff = strings.TrimRight(string(out), "\n")
	}
}

func isBinaryGitDiff(out []byte) bool {
	return bytes.HasPrefix(out, []byte("Binary files ")) ||
		bytes.Contains(out, []byte("\nBinary files ")) ||
		bytes.Contains(out, []byte("\nGIT binary patch\n"))
}

func trimUTF8Bytes(b []byte) string {
	s := strings.ToValidUTF8(string(b), "")
	// Trim any trailing partial line when truncated.
	if idx := strings.LastIndexByte(s, '\n'); idx > 0 {
		return s[:idx]
	}
	return s
}
