package hostclient

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// Diffs. Each git workspace's diff (against its base, with uncommitted and untracked changes, as
// the daemon parses it) goes up the stream when it changed, one workspace per frame. Reading a
// diff runs git, so a workspace's is read again only when its diff stat, branch or directory
// moved, or while an agent in it works (and once after, for its last edits).

// WorkspaceDiff is one workspace's diff. With Error the daemon could not read it (Files is empty);
// with TooLarge the daemon gave up on it whole. A file too big to send keeps its counts but loses
// its hunks (status too_large).
type WorkspaceDiff struct {
	WorkspaceID string           `json:"workspaceId"`
	Directory   string           `json:"directory"`
	CollectedAt time.Time        `json:"collectedAt"`
	Error       string           `json:"error,omitempty"`
	TooLarge    bool             `json:"tooLarge,omitempty"`
	Additions   int              `json:"additions"`
	Deletions   int              `json:"deletions"`
	Files       []paseo.DiffFile `json:"files"`
}

const (
	maxDiffBytes = 2 << 20 // a workspace's files, as JSON; the biggest lose their hunks past this
	maxDiffLine  = 4 << 10 // a line is cut after this many bytes
)

// DiffSync sends each git workspace's diff when it changes.
type DiffSync struct {
	Read func(ctx context.Context, cwd string) (paseo.CheckoutDiff, error)
	Send func(ctx context.Context, d WorkspaceDiff) error
	Now  func() time.Time

	mu      sync.Mutex
	sent    map[string][32]byte // what the server has: each workspace's diff, hashed
	stat    map[string]string   // each workspace's diff stat when its diff was last read
	working map[string]bool     // an agent worked in it when its diff was last read
}

// Reset forgets what the server has (a new stream), so the next Sync sends every diff.
func (s *DiffSync) Reset() {
	s.mu.Lock()
	s.sent, s.stat, s.working = nil, nil, nil
	s.mu.Unlock()
}

// Sync reads the diffs of snap's git workspaces that may have changed and sends those that did. A
// read that fails for want of the daemon is returned, and that workspace is read again next time.
func (s *DiffSync) Sync(ctx context.Context, snap PaseoStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !snap.Connected {
		return nil
	}
	if s.sent == nil {
		s.sent, s.stat, s.working = map[string][32]byte{}, map[string]string{}, map[string]bool{}
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	busy := map[string]bool{}
	for _, a := range snap.Agents {
		if working(a.Status) {
			busy[a.WorkspaceID] = true
		}
	}
	present := map[string]bool{}
	var errs []error
	for _, w := range snap.Workspaces {
		if w.Branch == "" || w.Directory == "" {
			continue // not a git checkout
		}
		present[w.ID] = true
		stat := fmt.Sprint(w.Directory, "\x00", w.Branch, "\x00", w.Dirty, w.Additions, w.Deletions)
		if _, ok := s.sent[w.ID]; ok && s.stat[w.ID] == stat && !busy[w.ID] && !s.working[w.ID] {
			continue
		}
		d, err := s.Read(ctx, w.Directory)
		var refused *paseo.CheckoutError
		if err != nil && !errors.As(err, &refused) {
			errs = append(errs, fmt.Errorf("%s: %w", w.ID, err))
			continue
		}
		wd := WorkspaceDiff{WorkspaceID: w.ID, Directory: w.Directory, CollectedAt: now(), TooLarge: d.TooLarge,
			Files: fitDiff(d.Files)}
		if refused != nil {
			wd.Error = refused.Message
		}
		for _, f := range wd.Files {
			wd.Additions += f.Additions
			wd.Deletions += f.Deletions
		}
		key := wd
		key.CollectedAt = time.Time{}
		b, _ := json.Marshal(key)
		sum := sha256.Sum256(b)
		if old, ok := s.sent[w.ID]; !ok || old != sum {
			if err := s.Send(ctx, wd); err != nil {
				return err
			}
			s.sent[w.ID] = sum
		}
		s.stat[w.ID], s.working[w.ID] = stat, busy[w.ID]
	}
	for id := range s.sent {
		if !present[id] {
			delete(s.sent, id)
			delete(s.stat, id)
			delete(s.working, id)
		}
	}
	return errors.Join(errs...)
}

func working(status string) bool { return status == "running" || status == "initializing" }

// fitDiff is files with long lines cut and, while they are over maxDiffBytes as JSON, the
// biggest files' hunks dropped. It never returns nil.
func fitDiff(files []paseo.DiffFile) []paseo.DiffFile {
	out := make([]paseo.DiffFile, len(files))
	size := make([]int, len(files))
	total := 0
	for i, f := range files {
		hunks := make([]paseo.DiffHunk, len(f.Hunks))
		for j, h := range f.Hunks {
			h.Lines = slices.Clone(h.Lines)
			for k := range h.Lines {
				h.Lines[k].Content = cutAt(h.Lines[k].Content, maxDiffLine)
			}
			hunks[j] = h
		}
		f.Hunks = hunks
		out[i] = f
		b, _ := json.Marshal(f)
		size[i] = len(b)
		total += len(b)
	}
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(size[b], size[a]) })
	for _, i := range order {
		if total <= maxDiffBytes {
			break
		}
		if len(out[i].Hunks) == 0 {
			continue
		}
		out[i].Hunks, out[i].Status = []paseo.DiffHunk{}, "too_large"
		b, _ := json.Marshal(out[i])
		total -= size[i] - len(b)
	}
	return out
}

// CheckoutDiff reads the diff of the checkout at cwd on the current connection.
func (w *Watcher) CheckoutDiff(ctx context.Context, cwd string) (paseo.CheckoutDiff, error) {
	c := w.current()
	if c == nil {
		return paseo.CheckoutDiff{}, errNoDaemon
	}
	return c.CheckoutDiff(ctx, cwd)
}
