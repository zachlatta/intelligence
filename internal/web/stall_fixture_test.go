package web

import (
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
)

// Each table shares one migrated database and account. Cases reset rows and drive
// their reports, command claims and engine passes without live workers or streams.
type stallFixture struct {
	d     dbtest.DB
	clock *fakeClock
	h     *testServer
}

func newStallFixture(t *testing.T) *stallFixture {
	t.Helper()
	d := dbtest.New(t)
	clock := &fakeClock{t: t0}
	h := newStallServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now })
	keyOf(t, h, "host-1")
	return &stallFixture{d: d, clock: clock, h: h}
}

func (f *stallFixture) reset(t *testing.T) *rawHost {
	t.Helper()
	for _, table := range []string{"workflow_runs", "workflows", "commands", "permission_resolutions", "hosts"} {
		if _, err := f.d.Owner.Exec(t.Context(), "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.mu.Lock()
	f.clock.t = t0
	f.clock.mu.Unlock()
	return stallReport(t, f.h, wfReport())
}
