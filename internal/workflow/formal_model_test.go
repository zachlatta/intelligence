package workflow

import (
	"fmt"
	"strings"
	"testing"
)

type queueRunStatus uint8

const (
	qQueued queueRunStatus = iota
	qStarting
	qRunning
	qFailed
	qCanceled
)

type queueState struct {
	run0             queueRunStatus
	run1             queueRunStatus
	scannedRun0      bool
	run0CanceledOnce bool
	startRetries     uint8
	retryPending     bool
	retryOverdue     bool
	hostReady        bool
	providerDisabled bool
	retrySpawned     uint8
	overdueStuck     bool
}

type queueBugFlags struct {
	startWithoutRecheck       bool
	allowDuplicateRetry       bool
	unboundedRetryHold        bool
	disabledProviderBlocksAll bool
}

type queueTransition struct {
	name string
	next queueState
}

func queueNextStates(s queueState, bugs queueBugFlags) []queueTransition {
	var out []queueTransition

	if !s.scannedRun0 && s.run0 == qQueued {
		n := s
		n.scannedRun0 = true
		out = append(out, queueTransition{"ScanQueue(run0)", n})
	}

	if s.run0 == qQueued || s.run0 == qStarting {
		n := s
		n.run0 = qCanceled
		n.run0CanceledOnce = true
		n.retryPending = false
		n.retryOverdue = false
		out = append(out, queueTransition{"Cancel(run0)", n})
	}

	if s.scannedRun0 {
		if s.run0 == qQueued || bugs.startWithoutRecheck {
			n := s
			n.scannedRun0 = false
			if s.providerDisabled && s.run0 == qQueued {
				if bugs.disabledProviderBlocksAll {
					// Disabled provider stays queued at head instead of failing.
					out = append(out, queueTransition{"StartPicked(run0,disabled-stays-queued)", n})
				} else {
					n.run0 = qFailed
					out = append(out, queueTransition{"StartPicked(run0,disabled->failed)", n})
				}
			} else if s.run0 == qQueued || (bugs.startWithoutRecheck && s.run0 == qCanceled) {
				n.run0 = qStarting
				out = append(out, queueTransition{"StartPicked(run0)", n})
			}
		} else {
			n := s
			n.scannedRun0 = false
			out = append(out, queueTransition{"StartPicked(run0,recheck-skipped)", n})
		}
	}

	if s.run0 == qStarting && !s.retryPending {
		n := s
		if s.startRetries < StartRetryLimit {
			n.retryPending = true
			out = append(out, queueTransition{"StartFail(run0,schedule-retry)", n})
		} else {
			n.run0 = qFailed
			out = append(out, queueTransition{"StartFail(run0,exhausted)", n})
		}
	}

	if s.run0 == qStarting && s.retryPending && !s.retryOverdue {
		n := s
		n.retryOverdue = true
		out = append(out, queueTransition{"RetryBecomesOverdue(run0)", n})
	}

	if s.run0 == qStarting && s.retryPending && s.retryOverdue {
		n := s
		if s.hostReady {
			n.retryPending = false
			n.retryOverdue = false
			n.startRetries++
			out = append(out, queueTransition{"ReconcileOverdueRetry(run0,relaunch)", n})
		} else if bugs.unboundedRetryHold {
			n.overdueStuck = true
			out = append(out, queueTransition{"ReconcileOverdueRetry(run0,stuck-forever)", n})
		} else {
			n.run0 = qFailed
			n.retryPending = false
			n.retryOverdue = false
			out = append(out, queueTransition{"ReconcileOverdueRetry(run0,fail)", n})
		}
	}

	if s.run0 == qFailed && s.retrySpawned < 2 {
		if s.retrySpawned == 0 || bugs.allowDuplicateRetry {
			n := s
			n.retrySpawned++
			out = append(out, queueTransition{"RetryFailedRun(run0)", n})
		}
	}

	if s.run1 == qQueued {
		run0Blocks := s.run0 == qStarting || s.run0 == qRunning || (s.run0 == qQueued && bugs.disabledProviderBlocksAll)
		if !run0Blocks {
			n := s
			n.run1 = qStarting
			out = append(out, queueTransition{"StartQueued(run1)", n})
		}
	}

	return out
}

func checkQueueModel(bugs queueBugFlags) (int, []string, string) {
	initials := []queueState{
		{run0: qQueued, run1: qQueued, hostReady: true, providerDisabled: false},
		{run0: qQueued, run1: qQueued, hostReady: false, providerDisabled: false},
		{run0: qQueued, run1: qQueued, hostReady: true, providerDisabled: true},
	}
	type node struct {
		state queueState
		trace []string
	}
	visited := make(map[queueState]bool)
	q := make([]node, 0, len(initials))
	for _, init := range initials {
		visited[init] = true
		q = append(q, node{state: init})
	}

	run1StartedWithDisabledRun0 := false
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		s := cur.state

		if s.run0CanceledOnce && (s.run0 == qStarting || s.run0 == qRunning) {
			return len(visited), cur.trace, "NoCanceledRunStarts violated (past bug 450cced): canceled run0 transitioned to starting/running"
		}
		if s.retrySpawned > 1 {
			return len(visited), cur.trace, "AtMostOneRetryPerFailedRun violated (past bug d36a461): failed run0 was retried more than once"
		}
		if s.overdueStuck {
			return len(visited), cur.trace, "BoundedOverdueRetryWait violated (past bug e33ff33): overdue start retry stayed stuck in starting"
		}
		if s.providerDisabled && !s.run0CanceledOnce && s.run1 == qStarting {
			run1StartedWithDisabledRun0 = true
		}

		for _, tr := range queueNextStates(s, bugs) {
			if !visited[tr.next] {
				visited[tr.next] = true
				trace := append(append([]string(nil), cur.trace...), tr.name)
				q = append(q, node{state: tr.next, trace: trace})
			}
		}
	}

	if !run1StartedWithDisabledRun0 {
		return len(visited), []string{"initial(run0=queued,providerDisabled=true,run1=queued)"},
			"DisabledProviderDoesNotBlockQueue violated (past bug 2381302): run1 never started behind disabled run0"
	}
	return len(visited), nil, ""
}

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

type stallBugFlags struct {
	allowReaskWithUndeliveredReply bool
	allowStallWithoutEpoch         bool
	unboundedContinuations         bool
}

type stallTransition struct {
	name string
	next stallState
}

func stallNextStates(s stallState, bugs stallBugFlags) []stallTransition {
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
		if s.reply == rFailed {
			if bugs.allowReaskWithUndeliveredReply {
				n := s
				n.status = sWaiting
				n.askedAgainToZach = true
				out = append(out, stallTransition{"OrchestratorAsk(re-asked-zach)", n})
			} else {
				n := s
				n.reply = rDelivered
				out = append(out, stallTransition{"OrchestratorAsk(answered-with-undelivered-reply)", n})
			}
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
		if !s.hasLifecycleEpoch {
			if bugs.allowStallWithoutEpoch {
				n := s
				n.continuedWithoutEpoch = true
				n.continuationsInEpoch++
				out = append(out, stallTransition{"Watchdog(no-epoch-continuation)", n})
			}
		} else if s.continuationsInEpoch < 2 || bugs.unboundedContinuations {
			if s.continuationsInEpoch < 3 {
				n := s
				n.continuationsInEpoch++
				if n.reply == rFailed {
					n.reply = rDelivered
				}
				out = append(out, stallTransition{fmt.Sprintf("WatchdogContinue(%d)", n.continuationsInEpoch), n})
			}
		} else {
			n := s
			n.status = sStalled
			out = append(out, stallTransition{"WatchdogMarkStalled", n})
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

func checkStallModel(bugs stallBugFlags) (int, []string, string) {
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
			return len(visited), cur.trace, "NoReaskWhenReplyUndelivered violated (past bugs 016fe16/666da5e): orchestrator re-asked Zach while a reply was undelivered"
		}
		if s.continuedWithoutEpoch {
			return len(visited), cur.trace, "NoContinuationWithoutLifecycleEpoch violated (past bug c0d6440): watchdog continued run with no lifecycle transition"
		}
		if s.continuationsInEpoch > 2 {
			return len(visited), cur.trace, "BoundedContinuationsPerEpoch violated (past bug 149a0a0): more than 2 continuations sent in one epoch"
		}
		if s.status == sStalled && s.continuationsInEpoch != 2 {
			return len(visited), cur.trace, "NoStallBeforeTwoContinuations violated (past bug 149a0a0): run marked stalled without 2 attempts"
		}

		for _, tr := range stallNextStates(s, bugs) {
			if !visited[tr.next] {
				visited[tr.next] = true
				trace := append(append([]string(nil), cur.trace...), tr.name)
				q = append(q, node{state: tr.next, trace: trace})
			}
		}
	}
	return len(visited), nil, ""
}

func TestFormalModelQueueAndRetries(t *testing.T) {
	states, trace, violation := checkQueueModel(queueBugFlags{})
	if violation != "" {
		t.Fatalf("%s\ntrace: %s", violation, strings.Join(trace, " -> "))
	}
	if states < 20 {
		t.Fatalf("explored only %d states; model space unexpectedly small", states)
	}
}

func TestFormalModelStallsAndReplyRedelivery(t *testing.T) {
	states, trace, violation := checkStallModel(stallBugFlags{})
	if violation != "" {
		t.Fatalf("%s\ntrace: %s", violation, strings.Join(trace, " -> "))
	}
	if states < 20 {
		t.Fatalf("explored only %d states; model space unexpectedly small", states)
	}
}

func TestFormalModelReplaysPastBugs(t *testing.T) {
	queueCases := []struct {
		name    string
		bugs    queueBugFlags
		wantSHA string
	}{
		{"450cced_start_canceled_run", queueBugFlags{startWithoutRecheck: true}, "450cced"},
		{"d36a461_duplicate_retry", queueBugFlags{allowDuplicateRetry: true}, "d36a461"},
		{"e33ff33_unbounded_overdue_retry", queueBugFlags{unboundedRetryHold: true}, "e33ff33"},
		{"2381302_disabled_provider_blocks_queue", queueBugFlags{disabledProviderBlocksAll: true}, "2381302"},
	}
	for _, tc := range queueCases {
		t.Run(tc.name, func(t *testing.T) {
			_, trace, violation := checkQueueModel(tc.bugs)
			if !strings.Contains(violation, tc.wantSHA) {
				t.Fatalf("expected violation naming %s, got %q (trace: %s)", tc.wantSHA, violation, strings.Join(trace, " -> "))
			}
			t.Logf("replayed %s: %s | trace: %s", tc.wantSHA, violation, strings.Join(trace, " -> "))
		})
	}

	stallCases := []struct {
		name    string
		bugs    stallBugFlags
		wantSHA string
	}{
		{"016fe16_reask_with_undelivered_reply", stallBugFlags{allowReaskWithUndeliveredReply: true}, "016fe16"},
		{"c0d6440_stall_without_lifecycle_epoch", stallBugFlags{allowStallWithoutEpoch: true}, "c0d6440"},
		{"149a0a0_unbounded_continuations", stallBugFlags{unboundedContinuations: true}, "149a0a0"},
	}
	for _, tc := range stallCases {
		t.Run(tc.name, func(t *testing.T) {
			_, trace, violation := checkStallModel(tc.bugs)
			if !strings.Contains(violation, tc.wantSHA) {
				t.Fatalf("expected violation naming %s, got %q (trace: %s)", tc.wantSHA, violation, strings.Join(trace, " -> "))
			}
			t.Logf("replayed %s: %s | trace: %s", tc.wantSHA, violation, strings.Join(trace, " -> "))
		})
	}
}
