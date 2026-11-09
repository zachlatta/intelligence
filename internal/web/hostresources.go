package web

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/fleet"
)

//go:embed hostresources.html
var hostResourcesHTML string

var hostFuncs = template.FuncMap{
	"gb":       gb,
	"hostLine": hostLine,
	"dur":      dur,
	"agentMem": func(h *fleet.HostNow, id string) string {
		if h == nil || h.AgentMem[id] == 0 {
			return ""
		}
		return gb(h.AgentMem[id])
	},
	"ago":  ago,
	"list": func(xs ...int) []int { return xs },
	"div":  func(a, b int) int { return a / b },
}

var hostResourcesTmpl = template.Must(template.New("hostresources").Funcs(hostFuncs).Parse(hostResourcesHTML + layoutHTML))

// gb is bytes in GB, one decimal.
func gb(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

// dur is CPU seconds as 3h 2m, 3m 0s or 4.2s.
func dur(secs float64) string {
	if secs < 60 {
		return fmt.Sprintf("%.1fs", secs)
	}
	return humanDuration(time.Duration(secs * float64(time.Second)))
}

// hostLine is a host's newest minute in a line: CPU, memory, disk, pressure.
func hostLine(h *fleet.HostNow) string {
	if h == nil || h.Minute == nil {
		return ""
	}
	m := h.Minute
	var parts []string
	if m.CPUAvgPct != nil {
		parts = append(parts, fmt.Sprintf("CPU %.0f%%", *m.CPUAvgPct))
	}
	if m.MemTotal > 0 {
		parts = append(parts, fmt.Sprintf("RAM %.1f/%s", float64(m.MemTotal-m.MemAvailableMin)/(1<<30), gb(m.MemTotal)))
	}
	if m.DiskTotal > 0 {
		parts = append(parts, fmt.Sprintf("disk %.0f%%", 100*h.DiskUsedFrac))
	}
	if m.PSIMemSome != nil && *m.PSIMemSome >= 1 {
		parts = append(parts, fmt.Sprintf("memory pressure %.0f%%", *m.PSIMemSome))
	}
	s := strings.Join(parts, " · ")
	if !h.Fresh {
		s += " · as of " + m.Minute.Format("Jan 2 15:04 MST")
	}
	return s
}

type hostCard struct {
	Host     string
	Now      *fleet.HostNow
	CPU, Mem template.HTML
	Agents   []fleet.AgentTotal
	OOMs     []fleet.OOMKill
}

// hostResourcesPage charts each host over the last ?hours= (default 24) and ranks its agents.
func hostResourcesPage(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hours := 24
		if v := r.URL.Query().Get("hours"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 24*90 {
				http.Error(w, "bad hours: want 1 to 2160", http.StatusBadRequest)
				return
			}
			hours = n
		}
		now := cfg.Now()
		from := now.Add(-time.Duration(hours) * time.Hour)
		res, err := reg.HostResources(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ooms, err := reg.OOMKills(r.Context(), from)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hosts, err := reg.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		names := map[string]bool{}
		var order []string
		for _, b := range hosts {
			names[b.Report.Name] = true
			order = append(order, b.Report.Name)
		}
		for name := range res {
			if !names[name] {
				order = append(order, name)
			}
		}
		var cards []hostCard
		for _, host := range order {
			h := res[host]
			if h == nil {
				continue
			}
			c := hostCard{Host: host, Now: h}
			hist, err := reg.HostHistory(r.Context(), host, from)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var cpuAvg, cpuMax, mem, paseo fleet.Series
			cpuAvg.Window, cpuMax.Window, mem.Window, paseo.Window = "CPU average", "CPU peak", "memory used", "Paseo's memory"
			for _, m := range hist {
				if m.CPUAvgPct != nil {
					cpuAvg.Points = append(cpuAvg.Points, fleet.Point{At: m.Minute, Used: *m.CPUAvgPct})
				}
				if m.CPUMaxPct != nil {
					cpuMax.Points = append(cpuMax.Points, fleet.Point{At: m.Minute, Used: *m.CPUMaxPct})
				}
				if m.MemTotal > 0 {
					mem.Points = append(mem.Points, fleet.Point{At: m.Minute, Used: 100 * float64(m.MemTotal-m.MemAvailableMin) / float64(m.MemTotal)})
					if m.PaseoMemMax != nil {
						paseo.Points = append(paseo.Points, fleet.Point{At: m.Minute, Used: 100 * float64(*m.PaseoMemMax) / float64(m.MemTotal)})
					}
				}
			}
			if len(hist) > 0 {
				c.CPU = usageChart(nonEmpty(cpuAvg, cpuMax), from, now)
				c.Mem = usageChart(nonEmpty(mem, paseo), from, now)
			}
			if c.Agents, err = reg.TopAgents(r.Context(), host, from); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, k := range ooms {
				if k.Host == host {
					c.OOMs = append(c.OOMs, k)
				}
			}
			cards = append(cards, c)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = hostResourcesTmpl.Execute(w, map[string]any{"Version": cfg.Version, "Short": short(cfg.Version), "Now": now,
			"Hours": hours, "Cards": cards})
	}
}

func nonEmpty(ss ...fleet.Series) []fleet.Series {
	var out []fleet.Series
	for _, s := range ss {
		if len(s.Points) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func hostResourcesAPI(reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := reg.HostResources(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, res)
	}
}
