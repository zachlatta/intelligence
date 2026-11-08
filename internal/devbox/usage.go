package devbox

import (
	"context"
	"log"
	"time"

	"github.com/zachlatta/intelligence/internal/usage"
)

// UsageSync reads the box's plans' usage every interval and sends each read to the server. A read
// that cannot be sent is tried again every RetryEvery until the next read replaces it.
type UsageSync struct {
	Read       func(context.Context) usage.Report
	Send       func(context.Context, usage.Report) error
	Every      time.Duration // between reads; default 5m
	RetryEvery time.Duration // between sends of a read the server has not stored; default 15s
}

// Run reads and sends until ctx ends.
func (u *UsageSync) Run(ctx context.Context) {
	every, retry := u.Every, u.RetryEvery
	if every == 0 {
		every = 5 * time.Minute
	}
	if retry == 0 {
		retry = 15 * time.Second
	}
	lastErr := ""
	for ctx.Err() == nil {
		next := time.Now().Add(every)
		rep := u.Read(ctx)
		for ctx.Err() == nil {
			sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := u.Send(sendCtx, rep)
			cancel()
			if err == nil {
				if lastErr != "" {
					log.Printf("usage: sending again")
				}
				lastErr = ""
				break
			}
			if msg := err.Error(); msg != lastErr && err != ErrNotConnected && ctx.Err() == nil {
				log.Printf("usage: %v", err)
				lastErr = msg
			}
			if !time.Now().Add(retry).Before(next) {
				break // the next read replaces this one
			}
			sleep(ctx, retry)
		}
		sleep(ctx, time.Until(next))
	}
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
