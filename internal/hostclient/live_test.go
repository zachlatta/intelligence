package hostclient

import (
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

// A running agent labelled live (a Grimm agent) has its transcript copied within a fraction of a
// second of each change, not only every LiveEvery, so the page that streams it keeps up.
func TestHostCopiesALiveAgentsTranscriptAtOnce(t *testing.T) {
	s, d := newStreamServer(t), paseotest.New(t, tok)
	d.SetWorkspaces(paseotest.Workspace("w1", "worktree", "running"))
	live := paseotest.Agent("g1", "w1", "running")
	live["labels"] = map[string]any{LiveLabel: "1"}
	d.SetAgents(live, paseotest.Agent("g2", "w1", "running"))
	d.SetTimeline("g1", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	d.SetTimeline("g2", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")))
	hostRig(t, s, d, func(c *Client) { c.LiveEvery = time.Hour; c.FastEvery = 20 * time.Millisecond })
	upload := func(agent string, seq int) bool {
		for _, f := range s.frames(TranscriptFrame) {
			for _, e := range f.Transcript.Entries {
				if f.Transcript.AgentID == agent && e.SeqStart == seq {
					return true
				}
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for !upload("g1", 1) || !upload("g2", 1) {
		if time.Now().After(deadline) {
			t.Fatal("never uploaded the first transcripts")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.SetTimeline("g1", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 2, paseotest.Message("assistant_message", "on it")))
	d.SetTimeline("g2", "e1", paseotest.Entry(1, 1, paseotest.Message("user_message", "hi")),
		paseotest.Entry(2, 2, paseotest.Message("assistant_message", "on it")))
	deadline = time.Now().Add(2 * time.Second)
	for !upload("g1", 2) {
		if time.Now().After(deadline) {
			t.Fatal("the live agent's new entry was not copied at once")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if upload("g2", 2) {
		t.Error("an agent not labelled live was copied before LiveEvery")
	}
}
