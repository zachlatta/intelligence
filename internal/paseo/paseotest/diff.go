package paseotest

import (
	"fmt"
	"strconv"

	"github.com/coder/websocket"
)

// DiffFile is a changed file as checkout.diff.get serves it: with status "binary" or "too_large"
// (the optional status) it has no hunks, else one hunk of a context line, deletions removed lines
// and additions added lines.
func DiffFile(path string, additions, deletions int, status ...string) map[string]any {
	st := "ok"
	if len(status) > 0 {
		st = status[0]
	}
	hunks := []any{}
	if st == "ok" {
		lines := []any{
			map[string]any{"type": "header", "content": fmt.Sprintf("@@ -1,%d +1,%d @@", deletions+1, additions+1)},
			map[string]any{"type": "context", "content": "package main", "tokens": []any{map[string]any{"text": "package", "style": "keyword"}}},
		}
		for i := range deletions {
			lines = append(lines, map[string]any{"type": "remove", "content": "removed line " + strconv.Itoa(i+1)})
		}
		for i := range additions {
			lines = append(lines, map[string]any{"type": "add", "content": "added line " + strconv.Itoa(i+1)})
		}
		hunks = append(hunks, map[string]any{"oldStart": 1, "oldCount": deletions + 1, "newStart": 1, "newCount": additions + 1,
			"lines": lines})
	}
	return map[string]any{"path": path, "isNew": false, "isDeleted": false, "additions": additions, "deletions": deletions,
		"hunks": hunks, "status": st}
}

type diff struct {
	files    []map[string]any
	tooLarge bool
}

// SetDiff sets the diff checkout.diff.get serves for the checkout at cwd (see DiffFile). A cwd
// with no diff set is not a git repository.
func (f *Daemon) SetDiff(cwd string, files ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diffs == nil {
		f.diffs = map[string]diff{}
	}
	f.diffs[cwd] = diff{files: files}
}

// SetDiffTooLarge makes the checkout at cwd's diff too large for the daemon to serve.
func (f *Daemon) SetDiffTooLarge(cwd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diffs == nil {
		f.diffs = map[string]diff{}
	}
	f.diffs[cwd] = diff{tooLarge: true}
}

// diff answers checkout.diff.get as Paseo 0.15 does; false for any other request. Called with
// f.mu held.
func (f *Daemon) diff(c *websocket.Conn, id string, msg map[string]any) bool {
	if msg["type"] != "checkout.diff.get.request" {
		return false
	}
	cwd, _ := msg["cwd"].(string)
	cmp, _ := msg["compare"].(map[string]any)
	switch mode, _ := cmp["mode"].(string); mode {
	case "uncommitted", "base", "all":
	default:
		f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": "Unknown request, try upgrading the daemon"}))
		return true
	}
	p := map[string]any{"requestId": id, "cwd": cwd, "files": []any{}, "error": nil}
	d, ok := f.diffs[cwd]
	switch {
	case !ok:
		p["error"] = map[string]any{"code": "NOT_GIT_REPO", "message": "Not a git repository: " + cwd}
	case d.tooLarge:
		p["diffTooLarge"] = true
	case len(d.files) > 0:
		p["files"] = d.files
	}
	f.write(c, session("checkout.diff.get.response", p))
	return true
}
