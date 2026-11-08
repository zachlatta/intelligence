// Package web serves Intelligence's HTTP interface.
package web

import (
	"cmp"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/fleet"
)

// Config is what the server needs.
type Config struct {
	Version     string          // the deployed commit, shown on every page and by /healthz
	Fleet       *fleet.Registry // dev boxes and their latest reports; nil = a fresh one
	DevboxToken string          // the bearer token dev boxes report with; "" refuses every report
	Now         func() time.Time
}

// maxReport bounds a report body; a real one is a few KB.
const maxReport = 1 << 20

//go:embed index.html
var indexHTML string

var indexTmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"ago":    ago,
	"agents": agentSummary,
}).Parse(indexHTML))

// New returns the app's HTTP handler.
func New(cfg Config) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Fleet == nil {
		cfg.Fleet = fleet.New(cfg.Now, time.Minute)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok", "version": cfg.Version})
	})
	mux.HandleFunc("GET /api/devboxes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, cfg.Fleet.List())
	})
	mux.HandleFunc("POST "+devbox.ReportPath, func(w http.ResponseWriter, r *http.Request) {
		report(cfg, w, r)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]any{
			"Version": cfg.Version,
			"Short":   short(cfg.Version),
			// set by tailscale serve for requests from a tailnet user
			"User":  cmp.Or(r.Header.Get("Tailscale-User-Name"), r.Header.Get("Tailscale-User-Login")),
			"Boxes": cfg.Fleet.List(),
			"Now":   cfg.Now(),
		})
	})
	return mux
}

// report records a dev box's report, if it carries the dev-box token.
func report(cfg Config, w http.ResponseWriter, r *http.Request) {
	if cfg.DevboxToken == "" {
		http.Error(w, "dev box reports are not configured on this server", http.StatusServiceUnavailable)
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(cfg.DevboxToken)) != 1 {
		http.Error(w, "bad or missing dev box token", http.StatusUnauthorized)
		return
	}
	var rep devbox.Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReport)).Decode(&rep); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !devbox.ValidName(rep.Name) {
		http.Error(w, "bad dev box name", http.StatusBadRequest)
		return
	}
	cfg.Fleet.Record(rep)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// short is a commit id cut to 12 characters for display; anything shorter is kept whole.
func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// ago says how long before now t was, in whole seconds, minutes, hours or days.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// agentSummary is agent counts as "2 running, 8 idle": running first, then the rest by name.
func agentSummary(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if (a == "running") != (b == "running") {
			if a == "running" {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", counts[k], k)
	}
	return strings.Join(parts, ", ")
}
