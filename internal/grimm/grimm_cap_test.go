package grimm

import (
	"strings"
	"testing"
	"time"
)

// managerMessages are the queued send_message commands addressed to the manager agent "mgr".
func (r *rig) managerMessages() []string {
	var out []string
	for _, c := range r.commands() {
		if c.Kind != "send_message" || c.Args["agentId"] != "mgr" {
			continue
		}
		text, _ := c.Args["text"].(string)
		out = append(out, text)
	}
	return out
}

// toldCap reports whether some message to the manager names the worker cap and its new value.
func toldCap(msgs []string, n string) bool {
	for _, m := range msgs {
		if strings.Contains(strings.ToLower(m), "cap") && strings.Contains(m, n) {
			return true
		}
	}
	return false
}

// Zach: changing the cap in the header tells the running manager the new cap, so it plans hires
// to it; the manager hears it once, while busy (it joins the turn) or when it next goes idle.
func TestChangingTheCapTellsTheRunningManager(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.ready() // manager "mgr" is running
	before := len(r.managerMessages())
	seven := 7
	if err := r.s.UpdateSession(t.Context(), r.acct, id, SessionUpdate{MaxWorkers: &seven}); err != nil {
		t.Fatal(err)
	}
	if got := r.one(`SELECT max_workers FROM grimm_sessions WHERE id = $1`, id); got != int32(7) {
		t.Fatalf("cap = %v, want 7", got)
	}
	// let the news reach the manager however it is delivered: mid-turn, or batched once idle
	r.reconcile(id)
	r.later(10 * time.Second)
	r.reconcile(id)
	r.setAgent("mgr", "idle")
	r.reconcile(id)
	r.later(10 * time.Second)
	r.reconcile(id)
	msgs := r.managerMessages()[before:]
	if !toldCap(msgs, "7") {
		t.Fatalf("the manager was not told the new cap of 7; messages to it: %q", msgs)
	}
	n := 0
	for _, m := range msgs {
		if strings.Contains(strings.ToLower(m), "cap") && strings.Contains(m, "7") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the manager was told the cap %d times, want once: %q", n, msgs)
	}
}

// An out-of-range cap is refused, changes nothing and tells the manager nothing.
func TestAnOutOfRangeCapIsRefusedAndNotSentToTheManager(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	id := r.ready()
	before := len(r.managerMessages())
	for _, n := range []int{0, -3, MaxWorkersLimit + 1} {
		v := n
		err := r.s.UpdateSession(t.Context(), r.acct, id, SessionUpdate{MaxWorkers: &v})
		if err == nil || !strings.Contains(err.Error(), "1 to") {
			t.Errorf("cap %d: err = %v, want a 1-to-limit error", n, err)
		}
	}
	if got := r.one(`SELECT max_workers FROM grimm_sessions WHERE id = $1`, id); got != int32(DefaultMaxWorkers) {
		t.Errorf("cap = %v after refused updates, want %d", got, DefaultMaxWorkers)
	}
	r.setAgent("mgr", "idle")
	r.later(10 * time.Second)
	r.reconcile(id)
	if msgs := r.managerMessages()[before:]; toldCap(msgs, "cap") {
		t.Errorf("a refused cap reached the manager: %q", msgs)
	}
}
