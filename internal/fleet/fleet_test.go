package fleet

import (
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/devbox"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func TestRecordedBoxesListByNameAndGoOfflineWhenStale(t *testing.T) {
	now := t0
	r := New(func() time.Time { return now }, time.Minute)
	r.Record(devbox.Report{Name: "zeta", Paseo: devbox.PaseoStatus{Daemon: "running"}})
	now = now.Add(30 * time.Second)
	r.Record(devbox.Report{Name: "alpha", Paseo: devbox.PaseoStatus{Daemon: "stopped"}})

	boxes := r.List()
	if len(boxes) != 2 || boxes[0].Report.Name != "alpha" || boxes[1].Report.Name != "zeta" {
		t.Fatalf("boxes = %+v, want alpha then zeta", boxes)
	}
	if !boxes[0].Online || !boxes[1].Online {
		t.Errorf("both reported within a minute: %+v", boxes)
	}
	if !boxes[1].LastSeen.Equal(t0) {
		t.Errorf("zeta last seen %v, want %v", boxes[1].LastSeen, t0)
	}

	now = t0.Add(61 * time.Second)
	boxes = r.List()
	if boxes[1].Online {
		t.Errorf("zeta silent for 61s with a 1m stale window should be offline")
	}
	if !boxes[0].Online {
		t.Errorf("alpha reported 31s ago and should be online")
	}
}

func TestLatestReportReplacesEarlier(t *testing.T) {
	r := New(func() time.Time { return t0 }, time.Minute)
	r.Record(devbox.Report{Name: "box", Paseo: devbox.PaseoStatus{Daemon: "running"}})
	r.Record(devbox.Report{Name: "box", Paseo: devbox.PaseoStatus{Daemon: "stopped"}})
	boxes := r.List()
	if len(boxes) != 1 || boxes[0].Report.Paseo.Daemon != "stopped" {
		t.Fatalf("boxes = %+v", boxes)
	}
}
