package host

import (
	"errors"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/probe"
)

var m0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func TestCollectorSummarizesAMinuteWithTheProbesCPU(t *testing.T) {
	c := &Collector{}
	f := func(v float64) *float64 { return &v }
	c.addSample(Sample{At: m0.Add(5 * time.Second), CPUPct: f(10), MemTotal: 100, MemAvailable: 50})
	c.addWalk(m0.Add(5*time.Second), map[string]AgentProcs{"a1": {Processes: 2, Mem: 100, CPUTicks: 50}})
	c.addWalk(m0.Add(20*time.Second), map[string]AgentProcs{"a1": {Processes: 3, Mem: 300, CPUTicks: 20}, "a2": {Processes: 1, Mem: 10}})
	// the first snapshot of an epoch only sets where counting starts
	r := c.finish(m0, &probe.Snapshot{Epoch: "e1", Agents: map[string]probe.Counters{"a1": {CPUNanos: 10e9, Execs: 5}}}, nil)
	if len(r.Minutes) != 1 || r.Minutes[0].Samples != 1 || r.Minutes[0].MemAvailableMin != 50 {
		t.Errorf("minutes = %+v", r.Minutes)
	}
	agents := map[string]AgentMinute{}
	for _, a := range r.Agents {
		agents[a.AgentID] = a
	}
	if a := agents["a1"]; a.MemMax != 300 || a.ProcessesMax != 3 || a.CPUSeconds != nil || a.CPUSource != "" {
		t.Errorf("a1 = %+v: no CPU until the probe has a minute", a)
	}
	if r.Probe == nil || !r.Probe.Running {
		t.Errorf("probe = %+v", r.Probe)
	}
	// next minute: the probe's counters rose; a3 ran only between walks, and the probe saw it
	c.addWalk(m0.Add(65*time.Second), map[string]AgentProcs{"a1": {Processes: 1, Mem: 50, CPUTicks: 99}})
	r = c.finish(m0.Add(time.Minute), &probe.Snapshot{Epoch: "e1", Seq: 1, Agents: map[string]probe.Counters{
		"a1": {CPUNanos: 13e9, Execs: 9}, "a3": {CPUNanos: 2e9, Execs: 40}},
		OOMs: []probe.OOM{{Seq: 1, PID: 7, Comm: "node", AgentID: "a1", AnonRSS: 1 << 30}}}, nil)
	agents = map[string]AgentMinute{}
	for _, a := range r.Agents {
		agents[a.AgentID] = a
	}
	if a := agents["a1"]; a.CPUSeconds == nil || *a.CPUSeconds != 3 || a.CPUSource != "ebpf" || a.Execs == nil || *a.Execs != 4 || a.MemMax != 50 {
		t.Errorf("a1 = %+v", a)
	}
	if a := agents["a3"]; a.CPUSeconds == nil || *a.CPUSeconds != 2 || *a.Execs != 40 || a.ProcessesMax != 0 {
		t.Errorf("a3 = %+v", a)
	}
	if len(r.OOMs) != 1 || r.OOMs[0].Comm != "node" || c.oomSeq != 1 {
		t.Errorf("ooms = %+v", r.OOMs)
	}
	// a new epoch restarts counting
	r = c.finish(m0.Add(2*time.Minute), &probe.Snapshot{Epoch: "e2", Agents: map[string]probe.Counters{"a1": {CPUNanos: 1e9}}}, nil)
	for _, a := range r.Agents {
		if a.CPUSeconds != nil {
			t.Errorf("CPU across epochs: %+v", a)
		}
	}
}

func TestCollectorWithoutTheProbeUsesProcTicks(t *testing.T) {
	c := &Collector{}
	c.addWalk(m0.Add(5*time.Second), map[string]AgentProcs{"a1": {Processes: 1, Mem: 1, CPUTicks: 150}})
	r := c.finish(m0, nil, errors.New("dial unix: no such file"))
	if len(r.Agents) != 1 || r.Agents[0].CPUSeconds == nil || *r.Agents[0].CPUSeconds != 1.5 || r.Agents[0].CPUSource != "proc" || r.Agents[0].Execs != nil {
		t.Errorf("agents = %+v", r.Agents)
	}
	if r.Probe == nil || r.Probe.Running || r.Probe.Error == "" {
		t.Errorf("probe = %+v", r.Probe)
	}
}

func TestPendingReportsMergeAndAreBounded(t *testing.T) {
	var p pending
	for i := range maxPendingMinutes + 10 {
		p.add(Report{Minutes: []Minute{{Minute: m0.Add(time.Duration(i) * time.Minute)}}, Agents: []AgentMinute{{AgentID: "a"}}})
	}
	r := p.report()
	if len(r.Minutes) != maxPendingMinutes || !r.Minutes[0].Minute.Equal(m0.Add(10*time.Minute)) {
		t.Errorf("kept %d minutes from %v", len(r.Minutes), r.Minutes[0].Minute)
	}
}
