package voicememo

import (
	"errors"
	"testing"
	"time"
)

// Run checks every second, so an error that persists (a PDW_INTELLIGENCE_ACCOUNT naming no
// account, a database outage) is logged once a minute, not every second; a new error, or the
// same one after a recovery, is logged at once.
func TestRepeatedErrorsAreLoggedOnceAMinute(t *testing.T) {
	start := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	missing, down := errors.New("missing account"), errors.New("database down")
	var l errorLog
	for _, tt := range []struct {
		after time.Duration
		err   error
		want  bool
	}{
		{0, missing, true},
		{time.Second, missing, false},
		{59 * time.Second, missing, false},
		{time.Minute, missing, true},
		{61 * time.Second, down, true},
		{62 * time.Second, missing, true},
		{63 * time.Second, nil, false},
		{64 * time.Second, missing, true},
	} {
		if got := l.report(tt.err, start.Add(tt.after)); got != tt.want {
			t.Errorf("+%v %v: logged=%v, want %v", tt.after, tt.err, got, tt.want)
		}
	}
}

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
