package devbox

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"devbox-1", "box_1", "a.b"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "has space", "<x>", "Upper", strings.Repeat("a", 65)} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
