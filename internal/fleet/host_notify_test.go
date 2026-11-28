package fleet_test

import (
	"context"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/resources"
)

func TestRecordHostNotifiesHostResourcesTopic(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	reg := fleet.New(d.Owner)
	ch := fleet.NewChanges(d.Owner)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go ch.Run(ctx)
	ch.Ready(ctx)

	wake1, stop1 := ch.Subscribe(fleet.HostResourcesTopic("host-1"))
	defer stop1()
	wake2, stop2 := ch.Subscribe(fleet.HostResourcesTopic("host-2"))
	defer stop2()

	now := time.Now().UTC().Truncate(time.Minute)
	cpu := 42.5
	rep := resources.Report{
		Minutes: []resources.Minute{{
			Minute:           now,
			Samples:          12,
			CPUs:             8,
			CPUAvgPct:        &cpu,
			MemTotal:         16 << 30,
			MemAvailableMin:  8 << 30,
			DiskTotal:        100 << 30,
			DiskAvailableMin: 50 << 30,
		}},
	}
	if err := reg.RecordHost(ctx, "host-1", rep, now); err != nil {
		t.Fatalf("RecordHost: %v", err)
	}

	select {
	case <-wake1:
	case <-time.After(3 * time.Second):
		t.Fatal("RecordHost did not notify HostResourcesTopic(host-1)")
	}
	select {
	case <-wake2:
		t.Fatal("RecordHost for host-1 unexpectedly notified host-2")
	case <-time.After(150 * time.Millisecond):
	}
}
