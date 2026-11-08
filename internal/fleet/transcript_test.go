package fleet

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

func entry(start, end int, item string) hostclient.TranscriptEntry {
	return hostclient.TranscriptEntry{SeqStart: start, SeqEnd: end, TurnID: "turn-1", Provider: "claude", At: t1, Item: json.RawMessage(item)}
}

const (
	userMsg  = `{"type": "user_message", "text": "fix the login"}`
	asstMsg  = `{"type": "assistant_message", "text": "on it"}`
	toolCall = `{"type": "tool_call", "callId": "c1", "name": "Bash", "status": "completed", "detail": {}, "error": null}`
)

func storeT(t *testing.T, r *Registry, b hostclient.TranscriptBatch) {
	t.Helper()
	if err := r.StoreTranscript(ctx, b, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestTranscriptStoresEntriesWithReadableColumns(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), time.Now())
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg), entry(2, 4, asstMsg), entry(5, 5, toolCall)}})
	var kind, tool, status, text string
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, `SELECT kind, tool, status, text FROM transcript_entries
		WHERE agent_id = 'a1' AND seq_start = 5`).Scan(&kind, &tool, &status, &text); err != nil {
		t.Fatal(err)
	}
	if kind != "tool_call" || tool != "Bash" || status != "completed" || text != "" {
		t.Errorf("tool call row: %q %q %q %q", kind, tool, status, text)
	}
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, `SELECT kind, text FROM transcript_entries WHERE agent_id = 'a1' AND seq_start = 2`).
		Scan(&kind, &text); err != nil || kind != "assistant_message" || text != "on it" {
		t.Errorf("message row: %q %q %v", kind, text, err)
	}
	tr, err := r.Transcript(ctx, "host-1", "a1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Epoch != "ep1" || len(tr.Entries) != 3 || tr.Entries[1].Kind != "assistant_message" || tr.Entries[1].Text != "on it" ||
		tr.Entries[2].Tool != "Bash" || !tr.Entries[0].At.Equal(t1) {
		t.Errorf("transcript = %+v", tr)
	}
}

func TestTranscriptAppendsReplacesTheLastEntryAndResets(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg), entry(2, 3, `{"type": "assistant_message", "text": "on"}`)}})
	// the streamed message grew: it is sent again whole, with what came after it
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep1",
		Entries: []hostclient.TranscriptEntry{entry(2, 4, asstMsg), entry(5, 5, toolCall)}})
	cur, err := r.TranscriptCursors(ctx, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if c := cur["a1"]; c.Epoch != "ep1" || c.LastSeqStart != 5 || c.LastSeqEnd != 5 {
		t.Errorf("cursor = %+v", c)
	}
	tr, _ := r.Transcript(ctx, "host-1", "a1", 0)
	if len(tr.Entries) != 3 || tr.Entries[1].Text != "on it" || tr.Entries[1].SeqEnd != 4 {
		t.Fatalf("entries = %+v", tr.Entries)
	}
	// a new epoch replaces everything
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep2", Replace: true,
		Entries: []hostclient.TranscriptEntry{entry(1, 1, `{"type": "user_message", "text": "start over"}`)}})
	tr, _ = r.Transcript(ctx, "host-1", "a1", 0)
	if tr.Epoch != "ep2" || len(tr.Entries) != 1 || tr.Entries[0].Text != "start over" {
		t.Fatalf("after reset = %+v", tr)
	}
}

func TestTranscriptAppendFromAnotherEpochIsRefused(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep1", Replace: true, Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg)}})
	err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep9",
		Entries: []hostclient.TranscriptEntry{entry(2, 2, asstMsg)}}, time.Now())
	if !errors.Is(err, ErrEpochChanged) {
		t.Fatalf("err = %v, want ErrEpochChanged", err)
	}
	if err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a2", Epoch: "ep1",
		Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg)}}, time.Now()); !errors.Is(err, ErrEpochChanged) {
		t.Fatalf("appending to a transcript that does not exist: err = %v, want ErrEpochChanged (send it whole)", err)
	}
}

func TestTranscriptOfAnAgentNotOnTheHostIsRefused(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	err := r.StoreTranscript(ctx, hostclient.TranscriptBatch{Host: "host-1", AgentID: "ghost", Epoch: "ep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg)}}, time.Now())
	if !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("err = %v, want ErrUnknownAgent", err)
	}
}

func TestTranscriptsOutliveADisconnectButNotTheirAgent(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep1", Replace: true, Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg)}})
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a3", Epoch: "ep1", Replace: true, Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg)}})
	record(t, r, hostclient.Report{Name: "host-1", Paseo: hostclient.PaseoStatus{Error: "daemon restarting"}}, time.Now())
	if cur, _ := r.TranscriptCursors(ctx, "host-1"); len(cur) != 2 {
		t.Fatalf("cursors after a disconnect = %v, want both transcripts kept", cur)
	}
	rep := full()
	rep.Paseo.Agents = rep.Paseo.Agents[:2] // a3 was archived
	record(t, r, rep, time.Now())
	cur, _ := r.TranscriptCursors(ctx, "host-1")
	if _, ok := cur["a3"]; ok || len(cur) != 1 {
		t.Fatalf("cursors = %v, want a3's transcript gone with its agent", cur)
	}
}

func TestListCountsEachAgentsTranscript(t *testing.T) {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), time.Now())
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep1", Replace: true,
		Entries: []hostclient.TranscriptEntry{entry(1, 1, userMsg), entry(2, 2, asstMsg)}})
	agents := list(t, r)[0].Workspaces[0].Agents
	if agents[0].ID != "a1" || agents[0].TranscriptEntries != 2 || !agents[0].TranscriptSynced {
		t.Errorf("a1 = %+v", agents[0])
	}
	if agents[1].TranscriptSynced {
		t.Errorf("a2 has no transcript: %+v", agents[1])
	}
}

func TestTranscriptStoresTextWithNULCharacters(t *testing.T) {
	d := dbtest.New(t)
	r := New(d.Owner)
	record(t, r, full(), time.Now())
	// tool output can hold NUL, which Postgres text and jsonb cannot; a literal backslash-u stays as it is
	storeT(t, r, hostclient.TranscriptBatch{Host: "host-1", AgentID: "a1", Epoch: "ep1", Replace: true, Entries: []hostclient.TranscriptEntry{
		entry(1, 1, `{"type": "assistant_message", "text": "a\u0000b \\u0000 c"}`)}})
	var text string
	if err := dbtest.Reader(t, d, "host-1").QueryRow(ctx, "SELECT text FROM transcript_entries WHERE agent_id = 'a1'").Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "a�b \\u0000 c" {
		t.Errorf("text = %q, want NUL as U+FFFD and the escaped text untouched", text)
	}
}
