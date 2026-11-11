package web

import (
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/fleet"
)

//go:embed usage.html
var usageHTML string

//go:embed plan.html
var planHTML string

// datedPlan is a plan with the time its resets and ages are measured from (the "plan" template).
type datedPlan struct {
	Login fleet.Plan
	Now   time.Time
}

var usageFuncs = template.FuncMap{
	"ago":       ago,
	"until":     until,
	"pct":       pct,
	"left":      left,
	"paceText":  paceText,
	"paceClass": paceClass,
	"join":      strings.Join,
	"bar":       bar,
	"short":     short,
	"planAt":    func(now time.Time, p fleet.Plan) datedPlan { return datedPlan{Login: p, Now: now} },
	"list":      func(xs ...int) []int { return xs },
}

var usageTmpl = template.Must(template.Must(template.New("usage").Funcs(usageFuncs).Parse(usageHTML)).Parse(planHTML + layoutHTML + modelGroupsHTML))

// usagePage shows every login's plan, a chart of each over the last ?days= (default 7), and how
// much of each week each login used.
func usagePage(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days := 7
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 365 {
				http.Error(w, "bad days: want 1 to 365", http.StatusBadRequest)
				return
			}
			days = n
		}
		now := cfg.Now()
		from := now.Add(-time.Duration(days) * 24 * time.Hour)
		plans, err := reg.Plans(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hist, err := reg.UsageHistory(r.Context(), from)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		peaks, err := reg.WeeklyPeaks(r.Context(), now.Add(-time.Duration(max(days, 56))*24*time.Hour))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type card struct {
			fleet.Plan
			Chart  template.HTML
			Legend []legend
			Peaks  []fleet.Cycle
		}
		var cards []card
		for _, p := range plans.Plans {
			c := card{Plan: p}
			var mine []fleet.Series
			for _, s := range hist {
				if s.Provider == p.Provider && s.Account == p.Account {
					mine = append(mine, s)
				}
			}
			if len(mine) > 0 {
				c.Chart = usageChart(mine, from, now)
				for i, s := range mine {
					c.Legend = append(c.Legend, legend{Window: s.Window, Color: chartColors[i%len(chartColors)]})
				}
			}
			for _, pk := range peaks {
				if pk.Provider == p.Provider && pk.Account == p.Account {
					c.Peaks = append(c.Peaks, pk)
				}
			}
			cards = append(cards, c)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = usageTmpl.Execute(w, map[string]any{
			"Version": cfg.Version, "Short": short(cfg.Version), "Now": now, "Days": days,
			"Cards": cards, "Problems": plans.Problems,
		})
	}
}

func usageAPI(reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := reg.Plans(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, v)
	}
}

type legend struct{ Window, Color string }

var chartColors = []string{"#4b70cc", "#e0803a", "#1f9d63", "#8a63d2", "#d04841", "#8e8b8a"}

// The chart's geometry: the plot is plotW by plotH at (chartLeft, chartTop), in a 640x150 host.
const (
	chartLeft, chartTop = 40.0, 10.0
	plotW, plotH        = 600.0, 120.0
)

// usageChart draws each series' used % from from to to: 0% at the bottom, 100% at the top.
func usageChart(series []fleet.Series, from, to time.Time) template.HTML {
	return valueChart(series, from, to, 100, func(v float64) string { return fmt.Sprintf("%.0f%%", v) }, from.UTC().Format("Jan 2"), "now")
}

// valueChart draws each series from from to to, 0 at the bottom and top at the top, the axis
// labelled by label, its start by fromLabel and its end by toLabel.
func valueChart(series []fleet.Series, from, to time.Time, top float64, label func(float64) string, fromLabel, toLabel string) template.HTML {
	span := to.Sub(from).Seconds()
	if span <= 0 {
		span = 1
	}
	if top <= 0 {
		top = 1
	}
	x := func(t time.Time) float64 {
		f := math.Max(0, math.Min(1, t.Sub(from).Seconds()/span))
		return chartLeft + f*plotW
	}
	y := func(v float64) float64 {
		return chartTop + plotH - math.Max(0, math.Min(top, v))/top*plotH
	}
	var b strings.Builder
	b.WriteString(`<svg class="chart" viewBox="0 0 640 150" role="img" aria-label="over time">`)
	for _, g := range []float64{0, top / 2, top} {
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" class="grid"/><text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
			chartLeft, chartLeft+plotW, y(g), y(g), chartLeft-4, y(g)+4, template.HTMLEscapeString(label(g)))
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="146" class="axis">%s</text><text x="%.1f" y="146" class="axis" text-anchor="end">%s</text>`,
		chartLeft, template.HTMLEscapeString(fromLabel), chartLeft+plotW, template.HTMLEscapeString(toLabel))
	for i, s := range series {
		pts := make([]string, 0, len(s.Points))
		for _, p := range s.Points {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(p.At), y(p.Used)))
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="1.5"><title>%s</title></polyline>`,
			strings.Join(pts, " "), chartColors[i%len(chartColors)], template.HTMLEscapeString(s.Window))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// until says how long from now t is, to the minute: "in 3d 12h", "in 40m".
func until(now, t time.Time) string {
	d := t.Sub(now).Round(time.Minute)
	if d <= 0 {
		return "now"
	}
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("in %dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("in %dh %dm", hours, mins)
	}
	return fmt.Sprintf("in %dm", mins)
}

func pct(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.0f%%", f)
	}
	return fmt.Sprintf("%.1f%%", f)
}

// left is a window's % left, or why there is none.
func left(w fleet.PlanWindow) string {
	if w.RemainingPct == nil {
		return "reset, not read since"
	}
	return pct(*w.RemainingPct) + " left"
}

// bar is the width of a window's remaining-% bar.
func bar(w fleet.PlanWindow) string {
	if w.RemainingPct == nil {
		return "0"
	}
	return fmt.Sprintf("%.0f", math.Max(0, math.Min(100, *w.RemainingPct)))
}

func paceText(w fleet.PlanWindow) string {
	if w.Pace == nil {
		return ""
	}
	p := math.Round(*w.Pace)
	if p >= 0 {
		return fmt.Sprintf("%.0f ahead of pace", p)
	}
	return fmt.Sprintf("%.0f behind pace", -p)
}

func paceClass(w fleet.PlanWindow) string {
	switch {
	case w.RemainingPct == nil || w.Stale:
		return "unknown"
	case *w.RemainingPct <= 0:
		return "out"
	case *w.RemainingPct <= 20 || (w.Pace != nil && *w.Pace < -10):
		return "low"
	}
	return "ok"
}
