package hostclient

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("line 1\nline 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md", "old.txt")
	runGit(t, dir, "commit", "-m", "initial")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestWorktreeDiffParse(t *testing.T) {
	t.Parallel()
	parsed, err := ParseCommand(WorktreeDiffKind, json.RawMessage(`{"workspaceId":"w1"}`))
	if err != nil {
		t.Fatalf("worktree_diff parse: %v", err)
	}
	if a, ok := parsed.(WorkspaceArgs); !ok || a.WorkspaceID != "w1" {
		t.Fatalf("parsed = %#v", parsed)
	}
	for _, bad := range []struct{ kind, args string }{
		{WorktreeDiffKind, `{}`},
		{WorktreeDiffKind, `{"workspaceId":"w1","title":"x"}`},
		{WorktreeDiffKind, `{"workspaceId":"w1","waitForAgent":"a1"}`},
		{ArchiveWorkspaceKind, `{"workspaceId":"w1","onlyIfClean":true}`},
	} {
		if _, err := ParseCommand(bad.kind, json.RawMessage(bad.args)); err == nil {
			t.Errorf("%s %s unexpectedly parsed", bad.kind, bad.args)
		}
	}
}

func TestWorktreeDiffCleanDirtyUnbornBinaryAndTruncation(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// 1. Clean repo
	cleanDir := t.TempDir()
	initGitRepo(t, cleanDir)

	// 2. Dirty repo: modified, staged new, deleted, untracked text, untracked binary
	dirtyDir := t.TempDir()
	initGitRepo(t, dirtyDir)
	if err := os.WriteFile(filepath.Join(dirtyDir, "README.md"), []byte("line 1\nline 2 updated\nline 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirtyDir, "staged.go"), []byte("package demo\n\nfunc Hi() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dirtyDir, "add", "staged.go")
	if err := os.Remove(filepath.Join(dirtyDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirtyDir, "notes.txt"), []byte("untracked line 1\nuntracked line 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirtyDir, "blob.bin"), []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Unborn HEAD (git init, no commits yet, staged + untracked)
	unbornDir := t.TempDir()
	runGit(t, unbornDir, "init")
	if err := os.WriteFile(filepath.Join(unbornDir, "first.txt"), []byte("staged before first commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, unbornDir, "add", "first.txt")
	if err := os.WriteFile(filepath.Join(unbornDir, "extra.txt"), []byte("untracked in unborn repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. Truncation (large untracked file > 32 KiB)
	bigDir := t.TempDir()
	initGitRepo(t, bigDir)
	bigContent := bytes.Repeat([]byte("0123456789abcdef0123456789abcdef\n"), 2000) // ~66 KiB
	if err := os.WriteFile(filepath.Join(bigDir, "huge.txt"), bigContent, 0o644); err != nil {
		t.Fatal(err)
	}

	d := daemon(t)
	ws := func(id, dir string) map[string]any {
		w := paseotest.Workspace(id, "worktree", "running")
		w["workspaceDirectory"] = dir
		return w
	}
	d.SetWorkspaces(ws("w-clean", cleanDir), ws("w-dirty", dirtyDir), ws("w-unborn", unbornDir), ws("w-big", bigDir))
	d.SetAgents()

	w := watcher(t, d)
	eventually(t, w, "workspaces", func(s PaseoStatus) bool { return len(s.Workspaces) == 4 })

	// Clean
	res, err := execute(t, w, WorktreeDiffKind, `{"workspaceId":"w-clean"}`)
	if err != nil {
		t.Fatalf("clean worktree_diff: %v", err)
	}
	if res.WorktreeDiff == nil || !res.WorktreeDiff.Clean || len(res.WorktreeDiff.Files) != 0 {
		t.Fatalf("clean result = %+v, want clean with 0 files", res.WorktreeDiff)
	}

	// Dirty
	res, err = execute(t, w, WorktreeDiffKind, `{"workspaceId":"w-dirty"}`)
	if err != nil {
		t.Fatalf("dirty worktree_diff: %v", err)
	}
	if res.WorktreeDiff == nil || res.WorktreeDiff.Clean {
		t.Fatalf("dirty result = %+v, want dirty", res.WorktreeDiff)
	}
	byPath := map[string]WorktreeDiffFile{}
	for _, f := range res.WorktreeDiff.Files {
		byPath[f.Path] = f
	}
	if f, ok := byPath["README.md"]; !ok || f.Status != "M" || f.Additions < 2 || f.Deletions < 1 || !strings.Contains(f.Diff, "+line 2 updated") {
		t.Errorf("README.md = %+v", f)
	}
	if f, ok := byPath["staged.go"]; !ok || f.Status != "A" || f.Additions < 3 || !strings.Contains(f.Diff, "+package demo") {
		t.Errorf("staged.go = %+v", f)
	}
	if f, ok := byPath["old.txt"]; !ok || f.Status != "D" || f.Deletions < 1 || !strings.Contains(f.Diff, "-remove me") {
		t.Errorf("old.txt = %+v", f)
	}
	if f, ok := byPath["notes.txt"]; !ok || f.Status != "?" || f.Additions != 2 || !strings.Contains(f.Diff, "+untracked line 1") {
		t.Errorf("notes.txt = %+v", f)
	}
	if f, ok := byPath["blob.bin"]; !ok || f.Status != "?" || !f.Binary || f.Diff != "" {
		t.Errorf("blob.bin = %+v, want binary without diff text", f)
	}

	// Unborn HEAD
	res, err = execute(t, w, WorktreeDiffKind, `{"workspaceId":"w-unborn"}`)
	if err != nil {
		t.Fatalf("unborn worktree_diff: %v", err)
	}
	if res.WorktreeDiff == nil || res.WorktreeDiff.Clean || len(res.WorktreeDiff.Files) != 2 {
		t.Fatalf("unborn result = %+v, want 2 files", res.WorktreeDiff)
	}

	// Truncation
	res, err = execute(t, w, WorktreeDiffKind, `{"workspaceId":"w-big"}`)
	if err != nil {
		t.Fatalf("big worktree_diff: %v", err)
	}
	if res.WorktreeDiff == nil || !res.WorktreeDiff.Truncated || len(res.WorktreeDiff.Files) != 1 || !res.WorktreeDiff.Files[0].Truncated {
		t.Fatalf("big result = %+v, want truncated", res.WorktreeDiff)
	}

	// Missing workspace
	res, err = execute(t, w, WorktreeDiffKind, `{"workspaceId":"w-missing"}`)
	if err != nil {
		t.Fatalf("missing worktree_diff err: %v", err)
	}
	if res.WorktreeDiff == nil || res.WorktreeDiff.Clean || res.WorktreeDiff.Error == "" {
		t.Fatalf("missing result = %+v, want structured error", res.WorktreeDiff)
	}
}
