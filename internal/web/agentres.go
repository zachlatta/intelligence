package web

import (
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"time"

	"github.com/zachlatta/intelligence/internal/fleet"
)

//go:embed resources.html
var resourcesHTML string

// agentResources is what the transcript page shows of an agent's CPU, memory and commands.
type agentResources struct {
	Base       string
	Minutes    int
	First      time.Time
	Last       time.Time
	CPUs       int
	HasCPU     bool
	CPUSeconds float64
	AvgCores   float64
	PeakCores  float64
	EBPF       bool
	MemNow     int64 // 0 without a minute in the last 3
	MemPeak    int64
	ProcsPeak  int
	HasExecs   bool
	Execs      int64
	CPU, Mem   template.HTML
	OOMs       []fleet.OOMKill
}

// size is bytes as 236 MB or 3.0 GB.
func size(b int64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	}
	return gb(b)
}

// buildResources sums an agent's minutes and charts its CPU (in cores) and memory over them,
// up to now while it is live.
func buildResources(u fleet.AgentUse, base string, now time.Time, isLive bool) agentResources {
	res := agentResources{Base: base, Minutes: len(u.Minutes), CPUs: u.CPUs, OOMs: u.OOMs}
	if len(u.Minutes) == 0 {
		return res
	}
	res.First, res.Last = u.Minutes[0].Minute, u.Minutes[len(u.Minutes)-1].Minute
	cpu := fleet.Series{Window: "CPU cores"}
	mem := fleet.Series{Window: "memory"}
	cpuMinutes := 0
	for _, m := range u.Minutes {
		if m.CPUSeconds != nil {
			res.HasCPU = true
			res.CPUSeconds += *m.CPUSeconds
			cpuMinutes++
			cores := *m.CPUSeconds / 60
			res.PeakCores = math.Max(res.PeakCores, cores)
			cpu.Points = append(cpu.Points, fleet.Point{At: m.Minute, Used: cores})
		}
		if m.CPUSource == "ebpf" {
			res.EBPF = true
		}
		if m.Execs != nil {
			res.HasExecs = true
			res.Execs += *m.Execs
		}
		res.MemPeak = max(res.MemPeak, m.MemMax)
		res.ProcsPeak = max(res.ProcsPeak, m.ProcessesMax)
		mem.Points = append(mem.Points, fleet.Point{At: m.Minute, Used: float64(m.MemMax)})
	}
	if cpuMinutes > 0 {
		res.AvgCores = res.CPUSeconds / float64(cpuMinutes*60)
	}
	if res.Last.After(now.Add(-3 * time.Minute)) {
		res.MemNow = u.Minutes[len(u.Minutes)-1].MemMax
	}
	const stamp = "Jan 2 15:04 MST"
	from, to, toLabel := res.First, res.Last.Add(time.Minute), res.Last.Add(time.Minute).Format(stamp)
	if isLive {
		to, toLabel = now, "now"
	}
	if len(cpu.Points) > 0 {
		top := math.Max(1, math.Ceil(res.PeakCores))
		res.CPU = valueChart([]fleet.Series{cpu}, from, to, top, func(v float64) string { return fmt.Sprintf("%g", v) },
			from.Format(stamp), toLabel)
	}
	if res.MemPeak > 0 {
		top := niceBytes(res.MemPeak)
		res.Mem = valueChart([]fleet.Series{mem}, from, to, top, func(v float64) string { return size(int64(v)) },
			from.Format(stamp), toLabel)
	}
	return res
}

// niceBytes is a chart top at or above b: 64 MB times a power of two, or 1.5 times that.
func niceBytes(b int64) float64 {
	for unit := float64(64 << 20); ; unit *= 2 {
		for _, k := range []float64{1, 1.5} {
			if float64(b) <= unit*k {
				return unit * k
			}
		}
	}
}

// agentUse reads and builds an agent's resources.
func agentUse(r *http.Request, cfg Config, reg *fleet.Registry, host, agentID string, isLive bool) (agentResources, error) {
	u, err := reg.AgentUse(r.Context(), host, agentID)
	if err != nil {
		return agentResources{}, err
	}
	return buildResources(u, "/hosts/"+host+"/agents/"+agentID, cfg.Now().UTC(), isLive), nil
}

// transcriptResources sends an agent's resources alone, for a live page to replace its own with.
func transcriptResources(cfg Config, reg *fleet.Registry, w http.ResponseWriter, r *http.Request) {
	host, agentID := r.PathValue("host"), r.PathValue("agent")
	agent, _, _, err := agentContext(r, reg, host, agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agent == nil {
		http.NotFound(w, r)
		return
	}
	res, err := agentUse(r, cfg, reg, host, agentID, live(agent))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = transcriptTmpl.ExecuteTemplate(w, "resources", res)
}
