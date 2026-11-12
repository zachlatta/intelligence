package voicememo

import (
	"testing"
	"time"
)

func TestNextPollIsEverySecondWithSlackAndBacksOff(t *testing.T) {
	start := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		took     time.Duration
		failures int
		want     time.Duration // after start
	}{
		// Due slightly before the next one-second tick, so a slightly late tick still polls.
		{"fast success", 80 * time.Millisecond, 0, 900 * time.Millisecond},
		// A slow PDW waits as long again as the read took: at most half its time goes to us.
		{"slow success", 3 * time.Second, 0, 6 * time.Second},
		{"first failure", 80 * time.Millisecond, 1, 80*time.Millisecond + 2*time.Second},
		{"third failure", 80 * time.Millisecond, 3, 80*time.Millisecond + 8*time.Second},
		{"capped failure", 80 * time.Millisecond, 20, 80*time.Millisecond + 64*time.Second},
	} {
		got := nextPoll(start, start.Add(tt.took), time.Second, tt.failures).Sub(start)
		if got != tt.want {
			t.Errorf("%s: next poll after %v, want %v", tt.name, got, tt.want)
		}
	}
}
