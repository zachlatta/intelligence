package hostclient

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// fakeDiffs is each checkout's diff for a DiffSync, and the diffs it sent.
type fakeDiffs struct {
	mu      sync.Mutex
	diffs   map[string]paseo.CheckoutDiff // by directory; one missing is not a git repository
	readErr error
	reads   []string
	sent    []WorkspaceDiff
}

func (f *fakeDiffs) sync() *DiffSync {
	return &DiffSync{
		Read: func(_ context.Context, cwd string) (paseo.CheckoutDiff, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reads = append(f.reads, cwd)
			if f.readErr != nil {
				return paseo.CheckoutDiff{}, f.readErr
			}
			d, ok := f.diffs[cwd]
			if !ok {
				return paseo.CheckoutDiff{}, &paseo.CheckoutError{Code: "NOT_GIT_REPO", Message: "Not a git repository: " + cwd}
			}
			return d, nil
		},
		Send: func(_ context.Context, d WorkspaceDiff) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.sent = append(f.sent, d)
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}
}

func (f *fakeDiffs) set(cwd string, files ...paseo.DiffFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffs[cwd] = paseo.CheckoutDiff{Files: files}
}

// take returns what was read and sent since the last take.
func (f *fakeDiffs) take() (reads []string, sent []WorkspaceDiff) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reads, sent = f.reads, f.sent
	f.reads, f.sent = nil, nil
	return reads, sent
}

func diffFile(path string, lines ...string) paseo.DiffFile {
	h := paseo.DiffHunk{OldStart: 1, NewStart: 1, NewCount: len(lines)}
	for _, l := range lines {
		h.Lines = append(h.Lines, paseo.DiffLine{Type: "add", Content: l})
	}
	return paseo.DiffFile{Path: path, IsNew: true, Additions: len(lines), Status: "ok", Hunks: []paseo.DiffHunk{h}}
}

func gitWorkspace(id, dir string, additions int) Workspace {
	return Workspace{ID: id, Name: id, Kind: "worktree", Directory: dir, Branch: "feature", Dirty: true, Additions: additions}
}

func TestDiffSyncSendsEachGitWorkspacesDiffOnceUntilItChanges(t *testing.T) {
	f := &fakeDiffs{diffs: map[string]paseo.CheckoutDiff{}}
	f.set("/w/a", diffFile("a.go", "one"))
	f.set("/w/b")
	s := f.sync()
	snap := PaseoStatus{Connected: true, Workspaces: []Workspace{gitWorkspace("a", "/w/a", 1), gitWorkspace("b", "/w/b", 0),
		{ID: "plain", Kind: "directory", Directory: "/w/plain"}}}
	if err := s.Sync(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	reads, sent := f.take()
	if strings.Join(reads, " ") != "/w/a /w/b" { // a directory outside git has no diff to read
		t.Errorf("reads = %v", reads)
	}
	if len(sent) != 2 || sent[0].WorkspaceID != "a" || sent[0].Directory != "/w/a" || len(sent[0].Files) != 1 ||
		sent[0].Files[0].Path != "a.go" || sent[0].Additions != 1 || sent[0].CollectedAt.IsZero() ||
		sent[1].WorkspaceID != "b" || sent[1].Files == nil || len(sent[1].Files) != 0 {
		t.Fatalf("sent = %+v", sent)
	}

	// nothing changed and no agent is working: nothing is read
	if err := s.Sync(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if reads, sent := f.take(); len(reads) != 0 || len(sent) != 0 {
		t.Errorf("unchanged: reads %v, sent %+v", reads, sent)
	}

	// a's diff stat moved: it is read again and sent
	f.set("/w/a", diffFile("a.go", "one", "two"))
	snap.Workspaces[0].Additions = 2
	if err := s.Sync(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if reads, sent := f.take(); strings.Join(reads, " ") != "/w/a" || len(sent) != 1 || sent[0].Additions != 2 {
		t.Errorf("after a change: reads %v, sent %+v", reads, sent)
	}

	// a new stream: the server may have lost them, so everything goes again
	s.Reset()
	if err := s.Sync(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if _, sent := f.take(); len(sent) != 2 {
		t.Errorf("after a reset: sent %+v", sent)
	}
}

func TestDiffSyncRereadsWhileAnAgentWorksAndOnceAfter(t *testing.T) {
	f := &fakeDiffs{diffs: map[string]paseo.CheckoutDiff{}}
	f.set("/w/a", diffFile("a.go", "one"))
	s := f.sync()
	snap := PaseoStatus{Connected: true, Workspaces: []Workspace{gitWorkspace("a", "/w/a", 1)},
		Agents: []Agent{{ID: "ag", WorkspaceID: "a", Status: "running"}}}
	ctx := context.Background()
	_ = s.Sync(ctx, snap)
	f.take()

	// the agent works: the diff is read each time, though the stat is the same, and sent when its content changed
	_ = s.Sync(ctx, snap)
	if reads, sent := f.take(); len(reads) != 1 || len(sent) != 0 {
		t.Errorf("running, unchanged: reads %v, sent %+v", reads, sent)
	}
	f.set("/w/a", diffFile("a.go", "uno"))
	_ = s.Sync(ctx, snap)
	if reads, sent := f.take(); len(reads) != 1 || len(sent) != 1 || sent[0].Files[0].Hunks[0].Lines[0].Content != "uno" {
		t.Errorf("running, changed: reads %v, sent %+v", reads, sent)
	}

	// the agent stops: one more read catches its last edits, then no more
	snap.Agents[0].Status = "idle"
	f.set("/w/a", diffFile("a.go", "eins"))
	_ = s.Sync(ctx, snap)
	if reads, sent := f.take(); len(reads) != 1 || len(sent) != 1 {
		t.Errorf("just stopped: reads %v, sent %+v", reads, sent)
	}
	_ = s.Sync(ctx, snap)
	if reads, _ := f.take(); len(reads) != 0 {
		t.Errorf("idle: reads %v", reads)
	}
}

func TestDiffSyncSendsTheDaemonsRefusalButNotAConnectionFailure(t *testing.T) {
	f := &fakeDiffs{diffs: map[string]paseo.CheckoutDiff{"/w/big": {TooLarge: true}}}
	s := f.sync()
	snap := PaseoStatus{Connected: true, Workspaces: []Workspace{gitWorkspace("gone", "/w/gone", 0), gitWorkspace("big", "/w/big", 0)}}
	if err := s.Sync(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	_, sent := f.take()
	if len(sent) != 2 || sent[0].Error != "Not a git repository: /w/gone" || sent[0].Files == nil || !sent[1].TooLarge {
		t.Fatalf("sent = %+v", sent)
	}

	f.readErr = errors.New("checkout.diff.get.request: paseo connection closed")
	snap.Workspaces = append(snap.Workspaces, gitWorkspace("new", "/w/new", 3))
	if err := s.Sync(context.Background(), snap); err == nil {
		t.Error("a connection failure was not returned")
	}
	if _, sent := f.take(); len(sent) != 0 {
		t.Errorf("sent on a connection failure: %+v", sent)
	}
	f.readErr = nil
	f.set("/w/new", diffFile("n.go", "x"))
	_ = s.Sync(context.Background(), snap) // tried again: it was never sent
	if _, sent := f.take(); len(sent) != 1 || sent[0].WorkspaceID != "new" {
		t.Errorf("retry: sent %+v", sent)
	}
}

func TestDiffSyncCutsADiffTooBigToSend(t *testing.T) {
	f := &fakeDiffs{diffs: map[string]paseo.CheckoutDiff{}}
	big := strings.Repeat("x", maxDiffLine+100)
	huge := make([]string, 0, 3000)
	for range 3000 {
		huge = append(huge, strings.Repeat("y", 1000))
	}
	f.set("/w/a", diffFile("long-line.txt", big), diffFile("huge.txt", huge...), diffFile("small.go", "ok"))
	s := f.sync()
	if err := s.Sync(context.Background(), PaseoStatus{Connected: true, Workspaces: []Workspace{gitWorkspace("a", "/w/a", 3002)}}); err != nil {
		t.Fatal(err)
	}
	_, sent := f.take()
	if len(sent) != 1 {
		t.Fatalf("sent = %d", len(sent))
	}
	files := sent[0].Files
	if l := files[0].Hunks[0].Lines[0].Content; len(l) > maxDiffLine+len(cutMark) || !strings.HasSuffix(l, cutMark) {
		t.Errorf("a long line was not cut: %d bytes", len(l))
	}
	if files[1].Status != "too_large" || len(files[1].Hunks) != 0 || files[1].Additions != 3000 {
		t.Errorf("the biggest file kept its hunks: %+v", files[1].Status)
	}
	if files[2].Status != "ok" || len(files[2].Hunks) != 1 {
		t.Errorf("a small file lost its hunks: %+v", files[2])
	}
}

func TestDiffSyncForgetsWorkspacesThatAreGone(t *testing.T) {
	f := &fakeDiffs{diffs: map[string]paseo.CheckoutDiff{}}
	f.set("/w/a", diffFile("a.go", "one"))
	s := f.sync()
	ctx := context.Background()
	_ = s.Sync(ctx, PaseoStatus{Connected: true, Workspaces: []Workspace{gitWorkspace("a", "/w/a", 1)}})
	_ = s.Sync(ctx, PaseoStatus{Connected: true, Workspaces: []Workspace{}})
	f.take()
	// back again (unarchived, or the daemon came back): the server dropped it with the report, so it is sent again
	_ = s.Sync(ctx, PaseoStatus{Connected: true, Workspaces: []Workspace{gitWorkspace("a", "/w/a", 1)}})
	if _, sent := f.take(); len(sent) != 1 {
		t.Errorf("sent = %+v", sent)
	}
}
