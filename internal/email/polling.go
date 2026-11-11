package email

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Gmail quotas: https://developers.google.com/workspace/gmail/api/reference/quota
// (2026-10-16): history.list=2, messages.list=5, messages.get(full)=60,
// getProfile=1. Idle polling at 1s costs 120 units/min (2% of 6,000/user/min).
// Charge every request at 50 units/s: bursts/backfill stay near 3,000/min,
// including pagination (a full-fetch request can add a 60-unit initial burst).
// Four workers keep steady backfill traffic near 200/s versus 20,000/project/s;
// pacing is per mailbox, not a shared project quota allocation. Other apps or
// server instances sharing the project/user still require reactive backoff.
// Deadlines are kept in gmail_connections under its existing row lock.
type pacer struct{ Next time.Time }

func (p *pacer) Wait(ctx context.Context, units int) error {
	t := time.NewTimer(max(time.Until(p.Next), 0))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	}
	p.Next = time.Now().Add(time.Duration(units) * time.Second / 50)
	return nil
}

type paceKey struct{}
type intervalKey struct{}

type retryError struct {
	Message string
	Delay   time.Duration
}

func (e *retryError) Error() string { return e.Message }

func retryAfter(value string, now time.Time) time.Duration {
	if n, err := strconv.ParseInt(value, 10, 32); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

func retryDelay(failures int, requested time.Duration) time.Duration {
	return max(time.Second*time.Duration(1<<min(failures, 6))+time.Duration(rand.Int64N(int64(time.Second))), requested)
}

func responseError(res *http.Response, message string, limited bool) error {
	if limited || res.StatusCode == 429 || res.StatusCode >= 500 {
		return &retryError{Message: message, Delay: retryAfter(res.Header.Get("Retry-After"), time.Now())}
	}
	return fmt.Errorf("%s", message)
}
