package workflow

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type stallRunStatus uint8

const (
	sRunning stallRunStatus = iota
	sWaiting
	sStalled
)

type replyDelivery uint8

const (
	rNone replyDelivery = iota
	rPending
	rFailed
	rDelivered
)

type stallState struct {
	status                stallRunStatus
	hasLifecycleEpoch     bool
	epoch                 uint8
	continuationsInEpoch  uint8
	daemonUp              bool
	reply                 replyDelivery
	askedAgainToZach      bool
	continuedWithoutEpoch bool
}

type stallTransition struct {
	name string
	next stallState
}

func stallNextStates(s stallState) []stallTransition {
	var out []stallTransition

	{
		n := s
		n.daemonUp = !s.daemonUp
		out = append(out, stallTransition{fmt.Sprintf("DaemonUp(%v)", n.daemonUp), n})
	}

	if s.status == sRunning && s.reply == rPending {
		n := s
		if s.daemonUp {
			n.reply = rDelivered
			out = append(out, stallTransition{"DeliverReply(succeeded)", n})
		} else {
			n.reply = rFailed
			out = append(out, stallTransition{"DeliverReply(failed-daemon-down)", n})
		}
	}

	if s.status == sRunning && s.reply != rPending {
		var undelivered []input
		if s.reply == rFailed {
			undelivered = []input{{Kind: "reply", Text: "proceed"}}
		}
		if answeredAsk(undelivered) {
			n := s
			n.reply = rDelivered
			out = append(out, stallTransition{"OrchestratorAsk(answered-with-undelivered-reply)", n})
		} else {
			n := s
			n.status = sWaiting
			out = append(out, stallTransition{"OrchestratorAsk(new-question)", n})
		}
	}

	if s.status == sWaiting && s.epoch < 2 {
		n := s
		n.status = sRunning
		n.hasLifecycleEpoch = true
		n.epoch++
		n.continuationsInEpoch = 0
		n.reply = rPending
		out = append(out, stallTransition{"ZachReply", n})
	}

	if s.status == sRunning && s.daemonUp && s.reply != rPending {
		for _, elapsed := range []time.Duration{time.Minute, 2 * time.Minute, 10 * time.Minute, 12 * time.Minute} {
			action, attempt := decideStallAction(s.hasLifecycleEpoch, true, int(s.continuationsInEpoch), elapsed)
			switch action {
			case stallContinue:
				n := s
				if !s.hasLifecycleEpoch {
					n.continuedWithoutEpoch = true
				}
				n.continuationsInEpoch = uint8(attempt)
				if n.reply == rFailed {
					n.reply = rDelivered
				}
				out = append(out, stallTransition{fmt.Sprintf("WatchdogContinue(%d@%s)", attempt, elapsed), n})
			case stallMarkStalled:
				n := s
				n.status = sStalled
				out = append(out, stallTransition{fmt.Sprintf("WatchdogMarkStalled(@%s)", elapsed), n})
			}
		}
	}

	if s.status == sStalled && s.daemonUp && s.epoch < 2 {
		n := s
		n.status = sRunning
		n.epoch++
		n.continuationsInEpoch = 1
		if n.reply == rFailed {
			n.reply = rDelivered
		}
		out = append(out, stallTransition{"Resume", n})
	}

	return out
}

func TestFormalModelStallsAndReplyRedelivery(t *testing.T) {
	initials := []stallState{
		{status: sRunning, hasLifecycleEpoch: true, epoch: 1, daemonUp: true},
		{status: sRunning, hasLifecycleEpoch: false, epoch: 0, daemonUp: true},
	}
	type node struct {
		state stallState
		trace []string
	}
	visited := make(map[stallState]bool)
	q := make([]node, 0, len(initials))
	for _, init := range initials {
		visited[init] = true
		q = append(q, node{state: init})
	}

	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		s := cur.state

		if s.askedAgainToZach {
			t.Fatalf("NoReaskWhenReplyUndelivered violated: %s", strings.Join(cur.trace, " -> "))
		}
		if s.continuedWithoutEpoch {
			t.Fatalf("NoContinuationWithoutLifecycleEpoch violated: %s", strings.Join(cur.trace, " -> "))
		}
		if s.continuationsInEpoch > 2 {
			t.Fatalf("BoundedContinuationsPerEpoch violated (%d > 2): %s", s.continuationsInEpoch, strings.Join(cur.trace, " -> "))
		}
		if s.status == sStalled && s.continuationsInEpoch != 2 {
			t.Fatalf("NoStallBeforeTwoContinuations violated (count=%d): %s", s.continuationsInEpoch, strings.Join(cur.trace, " -> "))
		}

		for _, tr := range stallNextStates(s) {
			if !visited[tr.next] {
				visited[tr.next] = true
				trace := append(append([]string(nil), cur.trace...), tr.name)
				q = append(q, node{state: tr.next, trace: trace})
			}
		}
	}
	if len(visited) < 20 {
		t.Fatalf("explored only %d states", len(visited))
	}
}
