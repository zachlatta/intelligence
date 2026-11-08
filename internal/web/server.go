// Package web serves Intelligence's HTTP interface.
package web

import (
	"cmp"
	"context"
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/fleet"
)

// Config is what the server needs. All app state is in DB, and Readonly reads it for the SQL API.
type Config struct {
	Version     string        // the deployed commit, shown on every page and by /healthz
	DB          *pgxpool.Pool // the app's database, migrated (db.Migrate)
	Readonly    *pgxpool.Pool // the same database as the read-only role (db.EnsureReadonlyRole)
	DevboxToken string        // the bearer token dev boxes report with; "" refuses every report
	SQLTimeout  time.Duration // per SQL API query; default 5s
	BinDir      string        // dev box binaries, intelligence-<os>-<arch>, served for self-update; "" = none
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
	if cfg.SQLTimeout == 0 {
		cfg.SQLTimeout = 5 * time.Second
	}
	reg := fleet.New(cfg.DB)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := cfg.DB.Ping(ctx); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "database unreachable", "version": cfg.Version})
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "version": cfg.Version, "database": "ok"})
	})
	mux.HandleFunc("GET /api/devboxes", func(w http.ResponseWriter, r *http.Request) {
		list, err := reg.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	})
	mux.HandleFunc("POST "+devbox.ReportPath, func(w http.ResponseWriter, r *http.Request) {
		report(cfg, reg, w, r)
	})
	mux.HandleFunc("GET /api/sql", sqlHandler(cfg))
	mux.HandleFunc("POST /api/sql", sqlHandler(cfg))
	mux.Handle("GET /api/devbox/binary/{os}/{arch}", binaries(cfg.BinDir))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		list, err := reg.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]any{
			"Version": cfg.Version,
			"Short":   short(cfg.Version),
			// set by tailscale serve for requests from a tailnet user
			"User":  cmp.Or(r.Header.Get("Tailscale-User-Name"), r.Header.Get("Tailscale-User-Login")),
			"Boxes": list,
			"Now":   cfg.Now(),
		})
	})
	// every response names the server's version, so a dev box sees on any call that it should update
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(devbox.VersionHeader, cfg.Version)
		mux.ServeHTTP(w, r)
	})
}

// report records a dev box's report, if it carries the dev-box token.
func report(cfg Config, reg *fleet.Registry, w http.ResponseWriter, r *http.Request) {
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
	if err := reg.Record(r.Context(), rep, cfg.Now()); err != nil {
		http.Error(w, "could not store the report: "+err.Error(), http.StatusInternalServerError)
		return
	}
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
