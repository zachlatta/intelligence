package host

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/zachlatta/intelligence/internal/probe"
)

// Report is what a box sends about itself: its minutes, each agent's minutes, the OOM kills the
// probe saw, and whether the probe runs.
type Report struct {
	Minutes []Minute      `json:"minutes,omitempty"`
	Agents  []AgentMinute `json:"agents,omitempty"`
	OOMs    []probe.OOM   `json:"ooms,omitempty"`
	Probe   *ProbeStatus  `json:"probe,omitempty"` // as of the newest minute
}

// AgentMinute is one agent's minute: its processes' most memory and count, and the CPU it used.
type AgentMinute struct {
	AgentID      string    `json:"agentId"`
	Minute       time.Time `json:"minute"`
	CPUSeconds   *float64  `json:"cpuSeconds,omitempty"`
	CPUSource    string    `json:"cpuSource,omitempty"` // ebpf: every process, exact; proc: processes alive at a walk
	MemMax       uint64    `json:"memMax"`
	ProcessesMax int       `json:"processesMax"`
	Execs        *uint64   `json:"execs,omitempty"` // programs started (the probe only)
}

// ProbeStatus says whether the box's eBPF probe answers.
type ProbeStatus struct {
	Running bool   `json:"running"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
	Tracked int    `json:"tracked,omitempty"`
}

// Collector samples the host and its agents and sends each finished minute.
type Collector struct {
	Reader      *Reader
	Walker      *AgentWalker
	ProbeSocket string // "" for none
	Send        func(context.Context, Report) error
	SampleEvery time.Duration // default 5s
	WalkEvery   time.Duration // default 15s

	samples  []Sample
	before   Sample // the last sample of the minute before
	walks    map[string]AgentProcs
	walkSeen bool
	epoch    string
	counters map[string]probe.Counters
	oomSeq   uint64
	queue    pending
}

func (c *Collector) addSample(s Sample) { c.samples = append(c.samples, s) }

// addWalk folds one walk into the minute: the most memory and processes, the ticks used.
func (c *Collector) addWalk(_ time.Time, w map[string]AgentProcs) {
	if c.walks == nil {
		c.walks = map[string]AgentProcs{}
	}
	c.walkSeen = true
	for id, a := range w {
		cur := c.walks[id]
		cur.Processes = max(cur.Processes, a.Processes)
		cur.Mem = max(cur.Mem, a.Mem)
		cur.CPUTicks += a.CPUTicks
		c.walks[id] = cur
	}
}

// finish closes the minute starting at m. snap is the probe's snapshot at its end (nil with
// probeErr when the probe did not answer).
func (c *Collector) finish(m time.Time, snap *probe.Snapshot, probeErr error) Report {
	var r Report
	if len(c.samples) > 0 {
		r.Minutes = []Minute{aggregate(m, c.samples, c.before)}
		c.before = c.samples[len(c.samples)-1]
	}
	c.samples = nil
	agents := map[string]*AgentMinute{}
	get := func(id string) *AgentMinute {
		if a := agents[id]; a != nil {
			return a
		}
		a := &AgentMinute{AgentID: id, Minute: m}
		agents[id] = a
		return a
	}
	for id, w := range c.walks {
		a := get(id)
		a.MemMax, a.ProcessesMax = w.Mem, w.Processes
	}
	switch {
	case snap != nil:
		r.Probe = &ProbeStatus{Running: true, Version: snap.Version, Tracked: snap.Tracked}
		if snap.Epoch == c.epoch {
			for id, now := range snap.Agents {
				was := c.counters[id]
				if now.CPUNanos < was.CPUNanos || now.Execs < was.Execs {
					continue
				}
				if now == was && agents[id] == nil {
					continue // idle and gone: no row
				}
				a := get(id)
				secs := float64(now.CPUNanos-was.CPUNanos) / 1e9
				execs := now.Execs - was.Execs
				a.CPUSeconds, a.CPUSource, a.Execs = &secs, "ebpf", &execs
			}
		} else {
			c.oomSeq = 0 // a new epoch numbers its kills from the start
		}
		for _, o := range snap.OOMs {
			if o.Seq > c.oomSeq {
				r.OOMs = append(r.OOMs, o)
				c.oomSeq = o.Seq
			}
		}
		c.epoch, c.counters = snap.Epoch, snap.Agents
	default:
		r.Probe = &ProbeStatus{Error: errString(probeErr)}
		c.epoch, c.counters = "", nil
		for id, w := range c.walks {
			secs := float64(w.CPUTicks) / 100 // USER_HZ
			a := get(id)
			a.CPUSeconds, a.CPUSource = &secs, "proc"
		}
	}
	c.walks = nil
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r.Agents = append(r.Agents, *agents[id])
	}
	return r
}

func errString(err error) string {
	if err == nil {
		return "probe not configured"
	}
	return err.Error()
}

// Run samples, walks and sends until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	every, walkEvery := c.SampleEvery, c.WalkEvery
	if every == 0 {
		every = 5 * time.Second
	}
	if walkEvery == 0 {
		walkEvery = 15 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	minute := time.Now().UTC().Truncate(time.Minute)
	var lastWalk time.Time
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now().UTC()
		if m := now.Truncate(time.Minute); m.After(minute) {
			var snap *probe.Snapshot
			var perr error
			if c.ProbeSocket != "" {
				fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				s, err := probe.Fetch(fctx, c.ProbeSocket, c.oomSeqFor())
				cancel()
				if err == nil {
					snap = &s
				}
				perr = err
			}
			c.queue.add(c.finish(minute, snap, perr))
			minute = m
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := c.Send(sctx, c.queue.report())
			cancel()
			if err == nil {
				c.queue = pending{}
				lastErr = ""
			} else if msg := err.Error(); msg != lastErr && ctx.Err() == nil {
				log.Printf("host: %v (keeping %d minutes to send)", err, len(c.queue.minutes))
				lastErr = msg
			}
		}
		if c.Reader != nil {
			c.addSample(c.Reader.Sample(now))
		}
		if c.Walker != nil && now.Sub(lastWalk) >= walkEvery {
			c.addWalk(now, c.Walker.Walk())
			lastWalk = now
		}
	}
}

// oomSeqFor is the seq to ask the probe for kills after: 0 until the epoch is known.
func (c *Collector) oomSeqFor() uint64 {
	if c.epoch == "" {
		return 0
	}
	return c.oomSeq
}

// maxPendingMinutes bounds the minutes kept while the server cannot be reached: three hours.
const maxPendingMinutes = 180

// pending is the minutes not yet stored by the server.
type pending struct {
	minutes []Minute
	agents  []AgentMinute
	ooms    []probe.OOM
	probe   *ProbeStatus
}

func (p *pending) add(r Report) {
	p.minutes = append(p.minutes, r.Minutes...)
	p.agents = append(p.agents, r.Agents...)
	p.ooms = append(p.ooms, r.OOMs...)
	if r.Probe != nil {
		p.probe = r.Probe
	}
	if n := len(p.minutes); n > maxPendingMinutes {
		p.minutes = p.minutes[n-maxPendingMinutes:]
		first := p.minutes[0].Minute
		kept := p.agents[:0]
		for _, a := range p.agents {
			if !a.Minute.Before(first) {
				kept = append(kept, a)
			}
		}
		p.agents = kept
	}
}

func (p *pending) report() Report {
	return Report{Minutes: p.minutes, Agents: p.agents, OOMs: p.ooms, Probe: p.probe}
}
