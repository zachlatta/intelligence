package devbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/usage"
)

func TestUsageSyncReadsEveryIntervalAndRetriesAFailedSend(t *testing.T) {
	var mu sync.Mutex
	reads, sends := 0, 0
	var sent []time.Time
	done := make(chan struct{})
	u := &UsageSync{Every: 60 * time.Millisecond, RetryEvery: 5 * time.Millisecond,
		Read: func(context.Context) usage.Report {
			mu.Lock()
			defer mu.Unlock()
			reads++
			return usage.Report{CollectedAt: time.Now()}
		},
		Send: func(_ context.Context, r usage.Report) error {
			mu.Lock()
			defer mu.Unlock()
			sends++
			if sends <= 2 {
				return ErrNotConnected // the stream is not up yet: send the same read again soon
			}
			sent = append(sent, r.CollectedAt)
			if len(sent) == 2 {
				close(done)
			}
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Run(ctx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never sent twice")
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 2 || sends != 4 || !sent[1].After(sent[0]) {
		t.Errorf("reads %d, sends %d, sent %v", reads, sends, sent)
	}
}

func TestUsageSyncRereadsWhenSendingFailsPastTheInterval(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	u := &UsageSync{Every: 20 * time.Millisecond, RetryEvery: 5 * time.Millisecond,
		Read: func(context.Context) usage.Report { mu.Lock(); reads++; mu.Unlock(); return usage.Report{} },
		Send: func(context.Context, usage.Report) error { return errors.New("down") }}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Millisecond)
	defer cancel()
	u.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if reads < 3 {
		t.Errorf("reads = %d: a read that cannot be sent goes stale and is read again", reads)
	}
}
