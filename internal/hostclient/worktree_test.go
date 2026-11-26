package hostclient

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func worktreeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWorkflowWorktreeBase(t *testing.T) {
	for _, tc := range []struct {
		name, branch string
		fetchRace    string
		offline      bool
		manual       bool
		base         string
	}{
		{name: "main", branch: "main"},
		{name: "renamed default", branch: "release/current"},
		{name: "concurrent fetch already updated ref", branch: "main", fetchRace: "updated"},
		{name: "concurrent fetch needs one retry", branch: "main", fetchRace: "retry"},
		{name: "fetch fails with a cached ref", branch: "main", offline: true},
		{name: "manual workspace", branch: "main", manual: true},
		{name: "explicit base", branch: "main", base: "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := filepath.Join("..", "..", ".factory-scratch")
			if err := os.MkdirAll(scratch, 0700); err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp(scratch, "worktree-base-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			dir, err = filepath.Abs(dir)
			if err != nil {
				t.Fatal(err)
			}
			origin := filepath.Join(dir, "origin.git")
			upstream := filepath.Join(dir, "upstream")
			checkout := filepath.Join(dir, "checkout")
			worktreeGit(t, dir, "init", "--bare", "--initial-branch=main", origin)
			worktreeGit(t, dir, "init", "--initial-branch=main", upstream)
			commit := func(message string) {
				worktreeGit(t, upstream, "-c", "user.name=Test", "-c", "user.email=test@example.com",
					"-c", "commit.gpgSign=false", "commit", "--allow-empty", "-m", message)
			}
			commit("old checkout")
			worktreeGit(t, upstream, "push", origin, "main")
			worktreeGit(t, dir, "clone", origin, checkout)
			old := worktreeGit(t, checkout, "rev-parse", "HEAD")
			worktreeGit(t, upstream, "switch", "-C", tc.branch)
			commit("new upstream tip")
			tip := worktreeGit(t, upstream, "rev-parse", "HEAD")
			worktreeGit(t, upstream, "push", origin, tc.branch)
			worktreeGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/"+tc.branch)
			if tc.offline {
				worktreeGit(t, checkout, "remote", "set-url", "origin", filepath.Join(dir, "missing.git"))
			}
			if tc.fetchRace != "" {
				simulateWorktreeFetchRace(t, dir, tc.fetchRace)
			}

			d := daemon(t)
			w := watcher(t, d)
			eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
			args := CreateWorkspaceArgs{Cwd: checkout, Worktree: true, Branch: "wf-factory-task", Base: tc.base}
			if !tc.manual {
				args.Agent = &CreateAgentArgs{Provider: "claude", Labels: map[string]string{
					"intelligence.run": "run", "intelligence.role": "orchestrator",
				}}
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			res, err := execute(t, w, CreateWorkspaceKind, string(raw))
			if err != nil || res.WorkspaceID == "" {
				t.Fatalf("workspace creation: %+v, %v", res, err)
			}
			replay := func(remote string) {
				t.Helper()
				worktreeGit(t, checkout, "remote", "set-url", "origin", remote)
				again, err := execute(t, w, CreateWorkspaceKind, string(raw))
				if err != nil || again != res {
					t.Fatalf("replay = %+v, %v, want %+v", again, err, res)
				}
			}
			requests := d.Requests("workspace.create.request")
			if len(requests) != 1 {
				t.Fatalf("got %d workspace requests", len(requests))
			}
			source := requests[0]["source"].(map[string]any)
			base, _ := source["baseBranch"].(string)
			if tc.offline || tc.manual || tc.base != "" {
				if base != tc.base {
					t.Fatalf("base = %q, want %q", base, tc.base)
				}
				if got := worktreeGit(t, checkout, "rev-parse", "refs/remotes/origin/main"); got != old {
					t.Fatalf("unexpected fetch: origin/main = %s, want %s", got, old)
				}
				if res.WorktreeBaseFallback != tc.offline {
					t.Fatalf("command result = %+v", res)
				}
				if tc.offline {
					replay(origin)
				}
				return
			}
			if want := "origin/" + tc.branch; base != want {
				t.Fatalf("Paseo received base %q, want %q", base, want)
			}
			if res.WorktreeBaseFallback {
				t.Fatalf("used the fetched base but reported fallback: %+v", res)
			}

			// Materialize the branch-off request using Git and the exact base sent to Paseo.
			created := filepath.Join(dir, "run")
			worktreeGit(t, checkout, "worktree", "add", "--no-track", "-b", args.Branch, created, base)
			if got := worktreeGit(t, created, "rev-parse", "HEAD"); got != tip {
				t.Fatalf("worktree HEAD = %s, want remote tip %s", got, tip)
			}
			if got := worktreeGit(t, created, "merge-base", "HEAD", base); got != tip {
				t.Fatalf("worktree merge-base = %s, want remote tip %s", got, tip)
			}
			if got := worktreeGit(t, checkout, "rev-parse", "HEAD"); got != old {
				t.Fatalf("source checkout moved to %s, want %s", got, old)
			}
			t.Logf("checkout HEAD = %s, %s and worktree merge-base = %s", old, base, tip)
			replay(filepath.Join(dir, "missing.git"))
		})
	}
}

// A second fetch can update the tracking ref before Git reports a lock or
// stale old value. Emulate those outcomes without relying on their timing.
func simulateWorktreeFetchRace(t *testing.T, dir, mode string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKTREE_TEST_GIT", git)
	t.Setenv("WORKTREE_TEST_FETCH_RACE", mode)
	t.Setenv("WORKTREE_TEST_FETCH_MARKER", filepath.Join(dir, "fetch-attempted"))
	shim := `#!/bin/sh
if [ "$1" = fetch ] && [ ! -e "$WORKTREE_TEST_FETCH_MARKER" ]; then
    touch "$WORKTREE_TEST_FETCH_MARKER" || exit 1
    if [ "$WORKTREE_TEST_FETCH_RACE" = updated ]; then
        "$WORKTREE_TEST_GIT" "$@" || exit 1
    fi
    echo "error: fetching ref failed: incorrect old value provided" >&2
    exit 1
fi
exec "$WORKTREE_TEST_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestReadGitFileEnforcesOriginDefaultBranch(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	upstream := filepath.Join(dir, "upstream")
	checkout := filepath.Join(dir, "checkout")
	worktreeGit(t, dir, "init", "--bare", "--initial-branch=main", origin)
	worktreeGit(t, dir, "init", "--initial-branch=main", upstream)

	writeFile := func(repo, rel, body string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	commitFile := func(repo, msg string) string {
		worktreeGit(t, repo, "add", "-A")
		worktreeGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgSign=false", "commit", "-m", msg)
		return worktreeGit(t, repo, "rev-parse", "HEAD")
	}

	const wantOutline = "- Root <!-- id:10000000-0000-4000-8000-000000000001 -->\n  - Alpha <!-- id:10000000-0000-4000-8000-000000000010 -->\n"
	writeFile(upstream, "outliner/outline.md", wantOutline)
	mainSHA := commitFile(upstream, "initial main")
	worktreeGit(t, upstream, "push", origin, "main")
	worktreeGit(t, dir, "clone", origin, checkout)

	// Push a second commit to origin/main that checkout has not fetched yet; ReadGitFile must fetch and verify it.
	const wantOutline2 = wantOutline + "    - Leaf <!-- id:10000000-0000-4000-8000-000000000011 -->\n"
	writeFile(upstream, "outliner/outline.md", wantOutline2)
	mainSHA2 := commitFile(upstream, "second main")
	worktreeGit(t, upstream, "push", origin, "main")

	// Create an unmerged commit on a side branch in checkout (never pushed to origin/main).
	worktreeGit(t, checkout, "checkout", "-b", "spoof-branch")
	writeFile(checkout, "outliner/outline.md", "- Fake <!-- id:99999999-0000-4000-8000-000000000001 -->\n")
	spoofSHA := commitFile(checkout, "spoof local branch")

	// Valid main commits succeed and return exact blob contents.
	for _, tc := range []struct {
		sha, want string
	}{
		{mainSHA, wantOutline},
		{mainSHA2, wantOutline2},
	} {
		got, err := ReadGitFile(t.Context(), ReadGitFileArgs{Directory: checkout, Commit: tc.sha, Path: "outliner/outline.md"})
		if err != nil {
			t.Fatalf("ReadGitFile(%s) failed: %v", tc.sha, err)
		}
		if got != tc.want {
			t.Fatalf("ReadGitFile(%s) = %q, want %q", tc.sha, got, tc.want)
		}
	}

	// Unmerged local branch commit MUST be refused (not on origin/main).
	if _, err := ReadGitFile(t.Context(), ReadGitFileArgs{Directory: checkout, Commit: spoofSHA, Path: "outliner/outline.md"}); err == nil || !strings.Contains(err.Error(), "not on") {
		t.Fatalf("ReadGitFile(off-main) = %v, want 'not on'", err)
	}

	// Missing file at valid commit MUST be refused.
	if _, err := ReadGitFile(t.Context(), ReadGitFileArgs{Directory: checkout, Commit: mainSHA2, Path: "outliner/missing.md"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ReadGitFile(missing) = %v, want 'not found'", err)
	}

	// ParseCommand refuses unsafe arguments.
	for _, raw := range []string{
		`{"directory":"relative","commit":"` + mainSHA + `","path":"outliner/outline.md"}`,
		`{"directory":"/srv/../etc","commit":"` + mainSHA + `","path":"outliner/outline.md"}`,
		`{"directory":"/srv/repo","commit":"main","path":"outliner/outline.md"}`,
		`{"directory":"/srv/repo","commit":"` + mainSHA + `","path":"../secret"}`,
		`{"directory":"/srv/repo","commit":"` + mainSHA + `","path":"HEAD:outliner/outline.md"}`,
	} {
		if _, err := ParseCommand(ReadGitFileKind, json.RawMessage(raw)); err == nil {
			t.Fatalf("ParseCommand accepted unsafe read_git_file args: %s", raw)
		}
	}
}
