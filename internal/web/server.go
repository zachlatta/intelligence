// Package web serves Intelligence's HTTP interface.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/fleet"
)

// Config is what the server needs. All app state is in DB; each account reads it through its own
// role, made with Readonly's settings (db.AccountRoles).
type Config struct {
	// Context is the server's lifetime: its listener for fleet changes stops when it ends. Default: never.
	Context    context.Context
	Version    string        // the deployed commit, shown on every page and by /healthz
	DB         *pgxpool.Pool // the app's database, migrated (db.Migrate)
	Readonly   *pgxpool.Pool // the same database as the read-only role (db.EnsureReadonlyRole)
	SQLTimeout time.Duration // per SQL API query; default 5s
	BinDir     string        // dev box binaries, intelligence-<os>-<arch>, served for self-update; "" = none
	Now        func() time.Time
}

// app is the server's shared parts.
type app struct {
	cfg     Config
	store   *accounts.Store
	reg     *fleet.Registry // as the owner: for the dev box streams and for writes
	changes *fleet.Changes
}

//go:embed index.html
var indexHTML string

var indexTmpl = template.Must(template.Must(template.New("index").Funcs(template.FuncMap{
	"ago":            ago,
	"agents":         agentSummary,
	"short":          short,
	"shortID":        func(id string) string { return id[:min(7, len(id))] }, // the prefix the paseo CLI accepts
	"plural":         plural,
	"allAgents":      allAgents,
	"idleWorkspaces": idleWorkspaces,
	"openWorkspace":  openWorkspace,
}).Funcs(usageFuncs).Funcs(hostFuncs).Parse(indexHTML)).Parse(planHTML + layoutHTML))

// New returns the app's HTTP handler.
func New(cfg Config) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SQLTimeout == 0 {
		cfg.SQLTimeout = 5 * time.Second
	}
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	reg := fleet.New(cfg.DB)
	changes := fleet.NewChanges(cfg.DB)
	go changes.Run(cfg.Context)
	roles := db.NewAccountRoles(cfg.DB, cfg.Readonly)
	context.AfterFunc(cfg.Context, roles.Close)
	store := accounts.New(cfg.DB, roles)
	a := &app{cfg: cfg, store: store, reg: reg, changes: changes}
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
	// accounts
	mux.HandleFunc("POST /api/accounts", a.signUp)
	mux.HandleFunc("POST /api/session", a.signIn)
	mux.HandleFunc("DELETE /api/session", a.signOut)
	mux.HandleFunc("PUT /api/account/password", a.write(a.changePassword))
	mux.HandleFunc("GET /login", loginPage(cfg))
	mux.HandleFunc("GET /ui.css", stylesheet)
	mux.HandleFunc("GET /account", a.page(func(v viewer) http.HandlerFunc { return accountPage(cfg, v) }))
	// dev boxes: joining an account, and their streams
	mux.HandleFunc("POST /api/devbox-enrollments", a.enroll)
	mux.HandleFunc("POST /api/devbox-enrollments/{id}/key", a.collectKey)
	mux.HandleFunc("POST /api/devbox-enrollments/claim", a.write(a.claimBox))
	mux.HandleFunc("DELETE /api/devboxes/{box}", a.write(a.removeBox))
	mux.HandleFunc("GET "+devbox.StreamPath("{box}"), stream(a))
	mux.Handle("GET /api/devbox/binary/{os}/{arch}", binaries(cfg.BinDir))
	// what a signed-in account reads: its own boxes, through its own role
	mux.HandleFunc("GET /api/devboxes", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			list, err := v.reg.List(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, list)
		}
	}))
	mux.HandleFunc("POST /api/devboxes/{box}/commands", a.write(func(v viewer) http.HandlerFunc { return enqueueCommand(a, v) }))
	mux.HandleFunc("GET /api/devboxes/{box}/commands/{id}", a.read(func(v viewer) http.HandlerFunc { return showCommand(a, v) }))
	mux.HandleFunc("GET /api/sql", a.read(func(v viewer) http.HandlerFunc { return sqlHandler(cfg, v.pool) }))
	mux.HandleFunc("POST /api/sql", a.read(func(v viewer) http.HandlerFunc { return sqlHandler(cfg, v.pool) }))
	mux.HandleFunc("GET /api/hosts", a.read(func(v viewer) http.HandlerFunc { return hostsAPI(v.reg) }))
	mux.HandleFunc("GET /api/usage", a.read(func(v viewer) http.HandlerFunc { return usageAPI(v.reg) }))
	mux.HandleFunc("GET /devboxes/{box}/workspaces/new", a.page(func(v viewer) http.HandlerFunc { return newWorkspacePage(cfg, v.reg) }))
	mux.HandleFunc("GET /devboxes/{box}/workspaces/{ws}/agents/new", a.page(func(v viewer) http.HandlerFunc { return newAgentPage(cfg, v.reg) }))
	mux.HandleFunc("GET /devboxes/{box}/agents/{agent}", a.page(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptPage(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /devboxes/{box}/agents/{agent}/blocks", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptBlocks(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /devboxes/{box}/agents/{agent}/resources", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptResources(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /devboxes/{box}/agents/{agent}/entries/{seq}", a.page(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptEntry(v.reg, w, r) }
	}))
	mux.HandleFunc("GET /usage", a.page(func(v viewer) http.HandlerFunc { return usagePage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts", a.page(func(v viewer) http.HandlerFunc { return hostsPage(cfg, v.reg) }))
	mux.HandleFunc("GET /{$}", a.page(func(v viewer) http.HandlerFunc { return homePage(cfg, v) }))
	// every response names the server's version, so a dev box sees on any call that it should update
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(devbox.VersionHeader, cfg.Version)
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// homePage is the tree of the account's hosts > workspaces > agents, and its plans.
func homePage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := v.reg.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		plans, err := v.reg.Plans(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hosts, err := v.reg.Hosts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]any{
			"Version":  cfg.Version,
			"Short":    short(cfg.Version),
			"Boxes":    list,
			"Plans":    plans,
			"Hosts":    hosts,
			"Now":      cfg.Now(),
			"Username": v.acct.Username,
		})
	}
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

// agentSummary counts agents by status, as "2 running, 8 idle": running first, then the rest by name.
func agentSummary(agents []fleet.AgentView) string {
	if len(agents) == 0 {
		return "no agents"
	}
	counts := map[string]int{}
	for _, a := range agents {
		counts[a.Status]++
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

// plural is "1 workspace", "3 workspaces", "2 entries".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if stem, ok := strings.CutSuffix(word, "y"); ok {
		return fmt.Sprintf("%d %sies", n, stem)
	}
	for _, end := range []string{"ch", "sh", "s", "x"} {
		if strings.HasSuffix(word, end) {
			return fmt.Sprintf("%d %ses", n, word)
		}
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// allAgents is every agent on box b, across its workspaces.
func allAgents(b fleet.Box) []fleet.AgentView {
	var out []fleet.AgentView
	for _, w := range b.Workspaces {
		out = append(out, w.Agents...)
	}
	return out
}

// idleWorkspaces are b's workspaces with no agents.
func idleWorkspaces(b fleet.Box) []fleet.WorkspaceAgents {
	var out []fleet.WorkspaceAgents
	for _, w := range b.Workspaces {
		if len(w.Agents) == 0 {
			out = append(out, w)
		}
	}
	return out
}

// openWorkspace says whether w starts open on the home page: it has an agent running or needing attention.
func openWorkspace(w fleet.WorkspaceAgents) bool {
	for _, a := range w.Agents {
		if a.Status == "running" || a.RequiresAttention {
			return true
		}
	}
	return false
}
