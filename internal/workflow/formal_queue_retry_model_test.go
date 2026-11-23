package workflow

import (
	"fmt"
	"strings"
	"testing"
)

// explore walks every state reachable from initial breadth-first and fails with the shortest
// trace to the first state that breaks an invariant. check returns "" for a good state.
func explore[S comparable](t *testing.T, initial []S, next func(S) []modelStep[S], check func(S) string, minStates int) {
	t.Helper()
	type node struct {
		state S
		trace []string
	}
	seen := make(map[S]bool)
	var q []node
	for _, s := range initial {
		seen[s] = true
		q = append(q, node{state: s})
	}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		if bad := check(cur.state); bad != "" {
			t.Fatalf("%s: %s", bad, strings.Join(cur.trace, " -> "))
		}
		for _, st := range next(cur.state) {
			if !seen[st.next] {
				seen[st.next] = true
				q = append(q, node{state: st.next, trace: append(append([]string(nil), cur.trace...), st.name)})
			}
		}
	}
	if len(seen) < minStates {
		t.Fatalf("explored only %d states, want at least %d", len(seen), minStates)
	}
}

type modelStep[S any] struct {
	name string
	next S
}

// The queue reads its runs without locking them, then locks the one it picked (startNext). A
// cancel can lock the run in between. The queue's check after the lock is stillInQueue.
type queuePC uint8

const (
	qIdle queuePC = iota
	qPicked
	qDone
)

type cancelPC uint8

const (
	cIdle cancelPC = iota
	cLocked
	cDone
)

type queueRaceState struct {
	status       string
	retryQueued  bool
	queue        queuePC
	cancel       cancelPC
	lockedBy     uint8 // 0 free, 1 queue, 2 cancel
	startedAfter bool  // started a run that a cancel had already ended
	canceled     bool
}

func queueRaceNext(s queueRaceState) []modelStep[queueRaceState] {
	var out []modelStep[queueRaceState]
	if s.queue == qIdle && (s.status == Queued || s.retryQueued) {
		n := s
		n.queue = qPicked
		out = append(out, modelStep[queueRaceState]{"QueueReadsUnlocked(picks run)", n})
	}
	if s.queue == qPicked && s.lockedBy == 0 {
		n := s
		n.queue = qDone
		if stillInQueue(s.status, s.retryQueued) {
			n.status = StartingOrchestrator
			n.retryQueued = false
			n.startedAfter = s.canceled
			out = append(out, modelStep[queueRaceState]{"QueueLocksAndStarts", n})
		} else {
			out = append(out, modelStep[queueRaceState]{"QueueLocksAndSkips", n})
		}
	}
	if s.cancel == cIdle && s.lockedBy == 0 {
		n := s
		n.cancel = cLocked
		n.lockedBy = 2
		out = append(out, modelStep[queueRaceState]{"CancelLocks", n})
	}
	if s.cancel == cLocked {
		n := s
		n.cancel = cDone
		n.lockedBy = 0
		if s.status == Queued || s.retryQueued {
			n.status = Canceled
			n.retryQueued = false
			n.canceled = true
			out = append(out, modelStep[queueRaceState]{"CancelCommits(canceled)", n})
		} else {
			out = append(out, modelStep[queueRaceState]{"CancelCommits(already left queue)", n})
		}
	}
	return out
}

func TestFormalModelQueueStartsNoCanceledRun(t *testing.T) {
	explore(t,
		[]queueRaceState{
			{status: Queued},
			{status: StartingOrchestrator, retryQueued: true},
		},
		queueRaceNext,
		func(s queueRaceState) string {
			if s.startedAfter {
				return "NoCanceledRunStarted violated"
			}
			return ""
		},
		10)
}

// Each retry request locks the failed run (lockRun), looks for an earlier retry of it, asks
// retryRefusal, and inserts the new run. The partial unique index on retried_from_run_id turns a
// second insert into a 500 instead of a second run.
type retryReq uint8

const (
	rqIdle retryReq = iota
	rqLocked
	rqCreated
	rqConflict
	rqServerError
)

const retryRequests = 3

type retryRaceState struct {
	reqs     [retryRequests]retryReq
	lockedBy int8 // -1 free
	retries  uint8
}

func retryRaceNext(s retryRaceState) []modelStep[retryRaceState] {
	var out []modelStep[retryRaceState]
	for i, r := range s.reqs {
		switch {
		case r == rqIdle && s.lockedBy == -1:
			n := s
			n.reqs[i] = rqLocked
			n.lockedBy = int8(i)
			out = append(out, modelStep[retryRaceState]{fmt.Sprintf("Retry%d.LocksRun", i), n})
		case r == rqLocked:
			n := s
			n.lockedBy = -1
			existing := ""
			if s.retries > 0 {
				existing = "earlier-retry"
			}
			switch {
			case retryRefusal(Failed, existing) != nil:
				n.reqs[i] = rqConflict
				out = append(out, modelStep[retryRaceState]{fmt.Sprintf("Retry%d.Refused(409)", i), n})
			case s.retries > 0:
				n.reqs[i] = rqServerError
				out = append(out, modelStep[retryRaceState]{fmt.Sprintf("Retry%d.InsertHitsUniqueIndex(500)", i), n})
			default:
				n.reqs[i] = rqCreated
				n.retries++
				out = append(out, modelStep[retryRaceState]{fmt.Sprintf("Retry%d.Inserts(201)", i), n})
			}
		}
	}
	return out
}

func TestFormalModelConcurrentRetriesStartOneRun(t *testing.T) {
	explore(t,
		[]retryRaceState{{lockedBy: -1}},
		retryRaceNext,
		func(s retryRaceState) string {
			if s.retries > 1 {
				return "OneRetryPerFailedRun violated"
			}
			for _, r := range s.reqs {
				if r == rqServerError {
					return "DuplicateRetryIsAConflictNotAnError violated"
				}
			}
			return ""
		},
		10)
}
