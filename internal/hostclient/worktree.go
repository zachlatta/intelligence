package hostclient

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

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
	for line := range strings.SplitSeq(head, "\n") {
		symref, isHead := strings.CutSuffix(line, "\tHEAD")
		branch, isBranch := strings.CutPrefix(symref, "ref: refs/heads/")
		if !isHead || !isBranch || branch == "HEAD" {
			continue
		}
		ref := "refs/heads/" + branch
		if _, err := git("check-ref-format", ref); err != nil {
			return ""
		}
		base := "origin/" + branch
		if _, err := git("fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--refmap=", "origin", "+"+ref+":refs/remotes/"+base); err != nil {
			return ""
		}
		return base
	}
	return ""
}
