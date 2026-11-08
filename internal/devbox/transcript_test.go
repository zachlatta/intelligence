package devbox

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// fakeServer keeps transcripts the way the real server does, and records each batch.
type fakeServer struct {
	mu      sync.Mutex
	store   map[string]*storedTranscript
	batches []TranscriptBatch
	known   map[string]bool // agents the server knows; others get 404
}

type storedTranscript struct {
	epoch   string
	entries map[int]TranscriptEntry
}

func newFakeServer(known ...string) *fakeServer {
	f := &fakeServer{store: map[string]*storedTranscript{}, known: map[string]bool{}}
	for _, k := range known {
		f.known[k] = true
	}
	return f
}

// cursors answers as the server's hello does.
func (f *fakeServer) cursors(context.Context) (map[string]TranscriptCursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]TranscriptCursor{}
	for id, s := range f.store {
		c := TranscriptCursor{Epoch: s.epoch}
		for _, e := range s.entries {
			if e.SeqStart > c.LastSeqStart {
				c.LastSeqStart, c.LastSeqEnd = e.SeqStart, e.SeqEnd
			}
		}
		out[id] = c
	}
	return out, nil
}

// send stores a batch as the server does, answering as its acks do.
func (f *fakeServer) send(_ context.Context, b TranscriptBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, b)
	if !f.known[b.AgentID] {
		return ErrNotYet
	}
	s := f.store[b.AgentID]
	if b.Replace {
		s = &storedTranscript{epoch: b.Epoch, entries: map[int]TranscriptEntry{}}
		f.store[b.AgentID] = s
	} else if s == nil || s.epoch != b.Epoch {
		return ErrResend
	}
	for _, e := range b.Entries {
		s.entries[e.SeqStart] = e
	}
	return nil
}

func (f *fakeServer) texts(agent string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.store[agent]
	if s == nil {
		return nil
	}
	var seqs []int
	for k := range s.entries {
		seqs = append(seqs, k)
	}
	slices.Sort(seqs)
	var out []string
	for _, k := range seqs {
		var item struct{ Text, Name string }
		json.Unmarshal(s.entries[k].Item, &item)
		out = append(out, item.Text+item.Name)
	}
	return out
}

func (f *fakeServer) batchCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.batches) }

type syncRig struct {
	d   *paseotest.Daemon
	c   *paseo.Client
	srv *fakeServer
	ts  *TranscriptSync
}

func newSyncRig(t *testing.T, known ...string) *syncRig {
	t.Helper()
	d := paseotest.New(t, tok)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := paseo.Dial(ctx, paseo.Target{URL: d.URL(), Token: tok, ClientID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := newFakeServer(known...)
	return &syncRig{d: d, c: c, srv: f, ts: &TranscriptSync{Box: "box", Timeline: c.Timeline, Cursors: f.cursors, Send: f.send}}
}

func ag(id, status string, updated time.Time) Agent {
	return Agent{ID: id, Status: status, UpdatedAt: updated}
}

func (r *syncRig) sync(t *testing.T, agents ...Agent) {
	t.Helper()
	if err := r.ts.Sync(context.Background(), agents); err != nil {
		t.Fatal(err)
	}
}

func (r *syncRig) timelineReads(agent string) []map[string]any {
	var out []map[string]any
	for _, q := range r.d.Requests("fetch_agent_timeline_request") {
		if q["agentId"] == agent {
			out = append(out, q)
		}
	}
	return out
}

func TestSyncUploadsLoadedAgentsAndNeverWakesClosedOnes(t *testing.T) {
	r := newSyncRig(t, "a1", "a2", "a3")
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 3, paseotest.Message("assistant_message", "hello")))
	r.d.SetTimeline("a2", "ep1", paseotest.Entry(1, 1, paseotest.ToolCall("Bash", "completed")))
	r.d.SetTimeline("a3", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "closed one")))
	r.sync(t, ag("a1", "idle", t0), ag("a2", "running", t0), ag("a3", "closed", t0))
	if got := r.srv.texts("a1"); strings.Join(got, "|") != "hi|hello" {
		t.Errorf("a1 = %v", got)
	}
	if got := r.srv.texts("a2"); strings.Join(got, "|") != "Bash" {
		t.Errorf("a2 = %v", got)
	}
	if n := len(r.timelineReads("a3")); n != 0 {
		t.Errorf("a closed agent's timeline was read %d times: that starts it again in Paseo", n)
	}
}

func TestSyncSkipsAnIdleAgentThatDidNotChange(t *testing.T) {
	r := newSyncRig(t, "a1")
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	r.sync(t, ag("a1", "idle", t0))
	r.sync(t, ag("a1", "idle", t0))
	if n := len(r.timelineReads("a1")); n != 1 {
		t.Errorf("timeline read %d times, want 1: nothing changed", n)
	}
}

func TestSyncSendsOnlyTheGrownTail(t *testing.T) {
	r := newSyncRig(t, "a1")
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 3, paseotest.Message("assistant_message", "hel")))
	r.sync(t, ag("a1", "running", t0))
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 5, paseotest.Message("assistant_message", "hello")),
		paseotest.Entry(6, 6, paseotest.ToolCall("Read", "completed")))
	r.sync(t, ag("a1", "running", t0.Add(time.Second)))
	if got := r.srv.texts("a1"); strings.Join(got, "|") != "hi|hello|Read" {
		t.Fatalf("a1 = %v", got)
	}
	reads := r.timelineReads("a1")
	cur, _ := reads[1]["cursor"].(map[string]any)
	if reads[1]["direction"] != "after" || cur["epoch"] != "ep1" || cur["seq"] != float64(1) {
		t.Errorf("second read = %v, want after seq 1 (re-reading the last entry, which grew)", reads[1])
	}
	last := r.srv.batches[len(r.srv.batches)-1]
	if last.Replace || len(last.Entries) != 2 {
		t.Errorf("last batch = replace %v with %d entries, want an append of 2", last.Replace, len(last.Entries))
	}
}

func TestSyncReplacesOnANewEpoch(t *testing.T) {
	r := newSyncRig(t, "a1")
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "old")),
		paseotest.Entry(2, 2, paseotest.Message("assistant_message", "older")))
	r.sync(t, ag("a1", "idle", t0))
	r.d.SetTimeline("a1", "ep2", paseotest.Entry(1, 1, paseotest.Message("user_message", "new")))
	r.sync(t, ag("a1", "idle", t0.Add(time.Second)))
	if got := r.srv.texts("a1"); strings.Join(got, "|") != "new" {
		t.Fatalf("a1 = %v, want the new epoch alone", got)
	}
}

func TestSyncResumesFromTheServersCursorsAfterARestart(t *testing.T) {
	r := newSyncRig(t, "a1")
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 2, paseotest.Message("assistant_message", "hello")))
	r.sync(t, ag("a1", "idle", t0))
	before := r.srv.batchCount()
	fresh := &TranscriptSync{Box: "box", Timeline: r.c.Timeline, Cursors: r.srv.cursors, Send: r.srv.send}
	if err := fresh.Sync(context.Background(), []Agent{ag("a1", "idle", t0)}); err != nil {
		t.Fatal(err)
	}
	reads := r.timelineReads("a1")
	if len(reads) != 2 || reads[1]["direction"] != "after" {
		t.Errorf("a restarted sync should continue from the server's cursor: %v", reads)
	}
	if n := r.srv.batchCount(); n != before {
		t.Errorf("a restarted sync with nothing new sent %d batches, want none", n-before)
	}
}

func TestSyncSplitsLargeTranscriptsIntoBatches(t *testing.T) {
	r := newSyncRig(t, "a1")
	var entries []map[string]any
	for i := 1; i <= 40; i++ {
		entries = append(entries, paseotest.Entry(i, i, paseotest.Message("assistant_message", strings.Repeat("x", 200))))
	}
	r.d.SetTimeline("a1", "ep1", entries...)
	r.ts.MaxBatch = 2000
	r.sync(t, ag("a1", "idle", t0))
	if n := r.srv.batchCount(); n < 4 {
		t.Fatalf("%d batches, want the transcript split", n)
	}
	for i, b := range r.srv.batches {
		if b.Replace != (i == 0) {
			t.Errorf("batch %d replace = %v: only the first starts the transcript over", i, b.Replace)
		}
	}
	if got := r.srv.texts("a1"); len(got) != 40 {
		t.Errorf("stored %d entries, want 40", len(got))
	}
}

func TestSyncResendsWholeWhenTheServerLostItsPlace(t *testing.T) {
	r := newSyncRig(t, "a1")
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	r.sync(t, ag("a1", "idle", t0))
	r.srv.mu.Lock()
	delete(r.srv.store, "a1") // the server dropped it (say, the agent briefly left a report)
	r.srv.mu.Unlock()
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 2, paseotest.Message("assistant_message", "back")))
	r.sync(t, ag("a1", "idle", t0.Add(time.Second)))
	if got := r.srv.texts("a1"); strings.Join(got, "|") != "hi|back" {
		t.Fatalf("a1 = %v, want it resent whole", got)
	}
}

func TestSyncRetriesAnAgentTheServerDoesNotKnowYet(t *testing.T) {
	r := newSyncRig(t)
	r.d.SetTimeline("a1", "ep1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	r.sync(t, ag("a1", "idle", t0)) // 404: its report has not landed
	r.srv.mu.Lock()
	r.srv.known["a1"] = true
	r.srv.mu.Unlock()
	r.sync(t, ag("a1", "idle", t0))
	if got := r.srv.texts("a1"); len(got) != 1 {
		t.Fatalf("a1 = %v, want it synced once the server knows the agent", got)
	}
}
