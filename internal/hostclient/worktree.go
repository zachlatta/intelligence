package hostclient

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxReadGitFileBytes caps read_git_file blob responses so they stay well within WebSocket frame limits.
const MaxReadGitFileBytes = 512 * 1024

// ReadGitFile refreshes origin's default branch in a.Directory, verifies a.Commit is an ancestor
// of that fetched origin default branch (e.g. origin/main), and reads a.Path directly from Git's
// object database without modifying the checkout or local branches.
func ReadGitFile(ctx context.Context, a ReadGitFileArgs) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	base := refreshWorkflowBase(ctx, a.Directory)
	if base == "" {
		return "", fmt.Errorf("could not resolve or fetch origin default branch in %s", a.Directory)
	}
	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = a.Directory
		cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
		cmd.WaitDelay = time.Second
		return cmd.Output()
	}

	trackingRef := "refs/remotes/" + base
	if _, err := git("merge-base", "--is-ancestor", "--", a.Commit, trackingRef); err != nil {
		return "", fmt.Errorf("commit %s is not on %s", a.Commit, base)
	}
	spec := a.Commit + ":" + a.Path
	sizeOut, err := git("cat-file", "-s", spec)
	if err != nil {
		return "", fmt.Errorf("file %s not found at commit %s", a.Path, a.Commit)
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(sizeOut)))
	if err != nil || size < 0 || size > MaxReadGitFileBytes {
		return "", fmt.Errorf("file %s at commit %s exceeds maximum size (%d bytes)", a.Path, a.Commit, MaxReadGitFileBytes)
	}
	blob, err := git("cat-file", "blob", spec)
	if err != nil {
		return "", fmt.Errorf("could not read %s at commit %s", a.Path, a.Commit)
	}
	if !utf8.Valid(blob) {
		return "", fmt.Errorf("file %s at commit %s is not valid UTF-8", a.Path, a.Commit)
	}
	return string(blob), nil
}

// refreshWorkflowBase resolves the remote's current default, since a clone's
// origin/HEAD can still point to an old default (or be missing altogether).
// Fetch exactly that branch even in a single-branch clone. Leave the checkout,
// local branches and fetch configuration alone.
func refreshWorkflowBase(ctx context.Context, cwd string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = cwd
		cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		return string(out), err
	}

	head, err := git("ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return ""
	}
	var branch, tip string
	for line := range strings.SplitSeq(head, "\n") {
		value, isHead := strings.CutSuffix(line, "\tHEAD")
		if !isHead {
			continue
		}
		if name, ok := strings.CutPrefix(value, "ref: refs/heads/"); ok {
			branch = name
		} else {
			tip = value
		}
	}
	if branch == "" || branch == "HEAD" || tip == "" {
		return ""
	}
	ref := "refs/heads/" + branch
	if _, err := git("check-ref-format", ref); err != nil {
		return ""
	}
	base := "origin/" + branch
	trackingRef := "refs/remotes/" + base
	for range 2 {
		if _, err := git("fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--refmap=", "origin", "+"+ref+":"+trackingRef); err == nil {
			return base
		}
		// Worktrees share the tracking ref. Another fetch may have already
		// advanced it, causing this fetch's ref update to fail.
		if _, err := git("merge-base", "--is-ancestor", "--", tip, trackingRef); err == nil {
			return base
		}
	}
	return ""
}
