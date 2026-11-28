package hostclient

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
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
	run("init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
}

func TestArchiveWorkspaceOnlyIfCleanParse(t *testing.T) {
	t.Parallel()
	parsed, err := ParseCommand(ArchiveWorkspaceKind, json.RawMessage(`{"workspaceId":"w1","onlyIfClean":true}`))
	if err != nil {
		t.Fatalf("archive_workspace onlyIfClean: %v", err)
	}
	if a, ok := parsed.(WorkspaceArgs); !ok || !a.OnlyIfClean || a.WorkspaceID != "w1" {
		t.Fatalf("parsed = %#v", parsed)
	}
	if _, err := ParseCommand(RenameWorkspaceKind, json.RawMessage(`{"workspaceId":"w1","title":"x","onlyIfClean":true}`)); err == nil {
		t.Fatal("rename_workspace accepted onlyIfClean")
	}
}

func TestArchiveWorkspaceOnlyIfCleanRemovesCleanWorktreeAndKeepsDirty(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cleanDir := t.TempDir()
	initGitRepo(t, cleanDir)

	dirtyDir := t.TempDir()
	initGitRepo(t, dirtyDir)
	if err := os.WriteFile(filepath.Join(dirtyDir, "untracked.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := daemon(t)
	w1 := paseotest.Workspace("w-clean", "worktree", "running")
	w1["workspaceDirectory"] = cleanDir
	w2 := paseotest.Workspace("w-dirty", "worktree", "running")
	w2["workspaceDirectory"] = dirtyDir
	d.SetWorkspaces(w1, w2)
	d.SetAgents(
		paseotest.Agent("a-clean", "w-clean", "idle"),
		paseotest.Agent("a-dirty", "w-dirty", "idle"),
	)

	w := watcher(t, d)
	eventually(t, w, "workspaces and agents", func(s PaseoStatus) bool { return len(s.Workspaces) == 2 && len(s.Agents) == 2 })

	res, err := execute(t, w, ArchiveWorkspaceKind, `{"workspaceId":"w-clean","onlyIfClean":true}`)
	if err != nil {
		t.Fatalf("clean archive: %v", err)
	}
	if res.WorktreeKept {
		t.Errorf("clean worktree was kept: %+v", res)
	}
	if got := len(d.Requests("archive_workspace_request")); got != 1 {
		t.Fatalf("clean archive_workspace_request count = %d, want 1", got)
	}

	res, err = execute(t, w, ArchiveWorkspaceKind, `{"workspaceId":"w-dirty","onlyIfClean":true}`)
	if err != nil {
		t.Fatalf("dirty archive: %v", err)
	}
	if !res.WorktreeKept {
		t.Errorf("dirty worktree was not marked kept: %+v", res)
	}
	if got := len(d.Requests("archive_workspace_request")); got != 1 {
		t.Fatalf("dirty worktree triggered archive_workspace_request: total %d, want 1", got)
	}
	// Agents in the dirty workspace were still archived.
	if res.ArchivedAgents != 1 {
		t.Errorf("dirty archivedAgents = %d, want 1", res.ArchivedAgents)
	}
}
