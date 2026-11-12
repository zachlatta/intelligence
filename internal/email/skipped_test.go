package email

import (
	"strings"
	"testing"
	"time"
)

// The error saying mail was skipped names the instant in RFC 3339, UTC, so every page can show it
// in its reader's own time zone, whatever the server's zone.
func TestSkippedMailErrorNamesTheInstantInUTC(t *testing.T) {
	at := time.Date(2026, 11, 11, 18, 57, 5, 0, time.FixedZone("EST", -5*3600))
	got := historySkipped(at).Error()
	if want := "email from before 2026-11-11T23:57:05Z was skipped"; !strings.Contains(got, want) {
		t.Errorf("error = %q, want it to contain %q", got, want)
	}
}
