package web

import (
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // schedules' times show in their own zones, whatever the image has

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// The schedule pages read what the hosts reported (schedules, schedule_runs) and change schedules
// through the commands API, following each command through the read-only SQL API. A host sends its
// schedules before a schedule command's result, so a page reloaded after one shows the change.

// scheduleCommandSQL is how the schedule pages follow a command ($ID): its status, its error and
// the schedule it made or changed.
const scheduleCommandSQL = `SELECT status, error, coalesce(result ->> 'scheduleId', '') AS schedule_id
	FROM commands WHERE id = '$ID'`

//go:embed schedules.html
var schedulesHTML string

var schedulesTmpl = template.Must(template.New("schedules").Funcs(template.FuncMap{
	"short": short, "ago": ago, "until": nextIn, "cadence": cadence, "inZone": inZone, "dur": runDuration,
	"localInput": localInput, "everyText": everyText,
}).Parse(spawnHTML + layoutHTML + schedulesHTML))

func renderSchedules(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := schedulesTmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// hostSchedules is one host and its schedules, for the schedules page.
type hostSchedules struct {
	Host      fleet.Host
	Problem   string // why the host could not list its schedules
	Schedules []fleet.ScheduleView
}

// schedulesPage lists every schedule on the account's hosts.
func schedulesPage(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hosts, err := reg.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		list, err := reg.Schedules(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		problems, err := reg.ScheduleProblems(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var groups []hostSchedules
		for _, b := range hosts {
			g := hostSchedules{Host: b, Problem: problems[b.Report.Name]}
			for _, s := range list {
				if s.Host == b.Report.Name {
					g.Schedules = append(g.Schedules, s)
				}
			}
			groups = append(groups, g)
		}
		renderSchedules(w, "schedules", map[string]any{"Version": cfg.Version, "Now": cfg.Now(), "Hosts": groups,
			"Count": len(list), "CommandSQL": scheduleCommandSQL})
	}
}

// schedulePage is one schedule: what it runs and when, its runs, and the form that edits it.
func schedulePage(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.PathValue("host")
		s, runs, err := reg.Schedule(r.Context(), host, r.PathValue("id"))
		if errors.Is(err, fleet.ErrNoSchedule) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b, _, err := hostNamed(reg, r, host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		renderSchedules(w, "schedule", map[string]any{"Version": cfg.Version, "Now": cfg.Now(), "Host": b, "S": s, "Runs": runs,
			"Providers": withCurrent(availableProviders(b), s.Provider), "Catalog": providerCatalog(b), "Thinking": thinkingLevels,
			"Commands": "/api/hosts/" + host + "/commands", "CommandSQL": scheduleCommandSQL, "Dictation": dictationPath(cfg)})
	}
}

// newSchedulePage is the form that makes a schedule on a host.
func newSchedulePage(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, ok, err := hostNamed(reg, r, r.PathValue("host"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		list, err := reg.Schedules(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data := spawnData(cfg, b)
		data["Directories"] = scheduleDirectories(b, list)
		data["CommandSQL"] = scheduleCommandSQL
		renderSchedules(w, "newschedule", data)
	}
}

// scheduleDirectories are the directories a schedule on b likely runs in: its repositories, the
// directories its other schedules use, then its plain directories and checkouts.
func scheduleDirectories(b fleet.Host, list []fleet.ScheduleView) []string {
	var out []string
	add := func(d string) {
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	for _, ws := range b.Workspaces {
		add(ws.ProjectRoot)
	}
	for _, s := range list {
		if s.Host == b.Report.Name {
			add(s.Cwd)
		}
	}
	for _, ws := range b.Workspaces {
		if ws.Listed && ws.Kind != "worktree" {
			add(ws.Directory)
		}
	}
	return out
}

// withCurrent is providers with cur in it (a schedule's provider the host no longer reports).
func withCurrent(providers []string, cur string) []string {
	if cur != "" && !slices.Contains(providers, cur) {
		providers = append(providers, cur)
	}
	return providers
}

var weekdays = map[string]string{"0": "Sundays", "1": "Mondays", "2": "Tuesdays", "3": "Wednesdays", "4": "Thursdays",
	"5": "Fridays", "6": "Saturdays", "7": "Sundays", "1-5": "weekdays", "0,6": "weekends", "6,0": "weekends"}

// cadence says when a schedule runs, in words where the cron expression is a common one.
func cadence(s hostclient.Schedule) string {
	if s.Cron == "" {
		if s.EveryMs <= 0 {
			return "never"
		}
		return "every " + everyText(s.EveryMs)
	}
	f := strings.Fields(s.Cron)
	if len(f) == 5 && f[2] == "*" && f[3] == "*" {
		m, errM := strconv.Atoi(f[0])
		h, errH := strconv.Atoi(f[1])
		switch {
		case errM == nil && f[1] == "*" && f[4] == "*":
			return fmt.Sprintf("hourly at :%02d", m)
		case errM == nil && errH == nil && f[4] == "*":
			return fmt.Sprintf("daily at %02d:%02d", h, m)
		case errM == nil && errH == nil && weekdays[f[4]] != "":
			return fmt.Sprintf("%s at %02d:%02d", weekdays[f[4]], h, m)
		}
	}
	return "cron " + s.Cron
}

// everyText is a duration in milliseconds as 30m, 2h or 1h30m.
func everyText(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// inZone is t in zone (a schedule's own; "" or one that does not load is UTC).
func inZone(t time.Time, zone string) string {
	if t.IsZero() {
		return ""
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		loc = time.UTC
	}
	return t.In(loc).Format("Mon Jan 2 15:04 MST")
}

// runDuration is how long a run took, or has taken so far.
func runDuration(now time.Time, r hostclient.ScheduleRun) string {
	if r.StartedAt.IsZero() {
		return ""
	}
	end := r.EndedAt
	if end.IsZero() {
		end = now
	}
	return humanDuration(end.Sub(r.StartedAt))
}

// localInput is t for a datetime-local input, in UTC (the page converts it to the browser's zone).
func localInput(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// nextIn is how long until t (until), "" for none.
func nextIn(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return until(now, t)
}
