package paseo

import (
	"context"
	"errors"
)

// The diff of a checkout: what a workspace changed, as the daemon parses it for its own diff view.

// DiffFile is one changed file. Status is "ok", "too_large" (no hunks) or "binary" (no hunks).
type DiffFile struct {
	Path      string     `json:"path"`
	OldPath   string     `json:"oldPath,omitempty"` // the path before a rename
	IsNew     bool       `json:"isNew"`
	IsDeleted bool       `json:"isDeleted"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Status    string     `json:"status,omitempty"`
	Hunks     []DiffHunk `json:"hunks"`
}

// DiffHunk is one hunk of a file's diff.
type DiffHunk struct {
	OldStart int        `json:"oldStart"`
	OldCount int        `json:"oldCount"`
	NewStart int        `json:"newStart"`
	NewCount int        `json:"newCount"`
	Lines    []DiffLine `json:"lines"`
}

// DiffLine is one line of a hunk: add, remove, context or header. Its highlighting is not read.
type DiffLine struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// CheckoutDiff is a checkout's diff. TooLarge: the daemon gave up on it whole (Files is empty).
type CheckoutDiff struct {
	Files    []DiffFile
	TooLarge bool
}

// CheckoutError is the daemon's refusal to read a checkout: not a git repository, and so on.
type CheckoutError struct{ Code, Message string }

func (e *CheckoutError) Error() string { return "checkout diff: " + e.Message }

// CheckoutDiff reads the diff of the checkout at cwd against its base branch (the merge base),
// with uncommitted and untracked changes: everything the checkout's work changed. The daemon's
// refusal (not a git checkout, ...) comes back as an error.
func (c *Client) CheckoutDiff(ctx context.Context, cwd string) (CheckoutDiff, error) {
	if cwd == "" {
		return CheckoutDiff{}, errors.New("checkout diff: no directory")
	}
	var p struct {
		Files []DiffFile `json:"files"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		DiffTooLarge bool `json:"diffTooLarge"`
	}
	req := map[string]any{"type": "checkout.diff.get.request", "cwd": cwd, "compare": map[string]any{"mode": "all"}}
	if err := c.request(ctx, req, &p); err != nil {
		return CheckoutDiff{}, err
	}
	if p.Error != nil {
		return CheckoutDiff{}, &CheckoutError{Code: p.Error.Code, Message: p.Error.Message}
	}
	for i := range p.Files {
		if p.Files[i].Hunks == nil {
			p.Files[i].Hunks = []DiffHunk{}
		}
	}
	if p.Files == nil {
		p.Files = []DiffFile{}
	}
	return CheckoutDiff{Files: p.Files, TooLarge: p.DiffTooLarge}, nil
}
