package workflow

import (
	"context"
	"testing"
	"time"
)

func TestStallWorkerReconcilesBeforeTickAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time, 1)
	calls := 0
	runStallWatchdog(ctx, ticks, func(context.Context) error {
		calls++
		if calls == 1 {
			// The initial reconciliation must happen without receiving a tick.
			ticks <- time.Time{}
		} else {
			cancel()
		}
		return nil
	})
	if calls != 2 {
		t.Fatalf("reconciliations=%d, want initial and one controlled tick", calls)
	}
}
