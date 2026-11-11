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
	"github.com/zachlatta/intelligence/internal/email"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/voicememo"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// Config is what the server needs. All app state is in DB; each account reads it through its own
// role, made with Readonly's settings (db.AccountRoles).
type Config struct {
	PDWURL, PDWKey, PDWAccount string
	// Context is the server's lifetime: its listener for fleet changes stops when it ends. Default: never.
	Context    context.Context
	Version    string        // the deployed commit, shown on every page and by /healthz
	DB         *pgxpool.Pool // the app's database, migrated (db.Migrate)
	Readonly   *pgxpool.Pool // the same database as the read-only role (db.EnsureReadonlyRole)
	SQLTimeout time.Duration // per SQL API query; default 5s
	BinDir     string        // host binaries, intelligence-<os>-<arch>, served for self-update; "" = none
	// AssemblyAIKey mints the chat box's dictation tokens; "" turns dictation off.
	AssemblyAIKey      string
	AssemblyAITokenURL string // default AssemblyAI's own
	Now                func() time.Time
	// Google (GOOGLE_OAUTH_CLIENT_JSON) and EmailKey (CONNECTIONS_KEY) turn email triggers on: an
	// account connects a Gmail mailbox, read every EmailInterval (default email.Interval).
	Google        *email.Google
	EmailKey      *email.Keyring
	EmailInterval time.Duration
	PublicURL     string // PUBLIC_URL: the server's origin for Google's redirect; default the request's
}

// app is the server's shared parts.
type app struct {
	cfg     Config
	store   *accounts.Store
	reg     *fleet.Registry // as the owner: for the host streams and for writes
	changes *fleet.Changes
	wf      *workflow.Engine // as the owner: workflows and their runs
	mail    *email.Service   // reads connected mailboxes; nil when email triggers are off
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
	"running":        runningAgents,
	"working":        working,
	"losesChanges":   losesChanges,
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
	if cfg.AssemblyAITokenURL == "" {
		cfg.AssemblyAITokenURL = assemblyAITokenURL
	}
	reg := fleet.New(cfg.DB)
	changes := fleet.NewChanges(cfg.DB)
	go changes.Run(cfg.Context)
	roles := db.NewAccountRoles(cfg.DB, cfg.Readonly)
	context.AfterFunc(cfg.Context, roles.Close)
	store := accounts.New(cfg.DB, roles)
	a := &app{cfg: cfg, store: store, reg: reg, changes: changes, wf: workflow.NewEngine(cfg.DB, cfg.Now)}
	if cfg.PDWURL != "" && cfg.PDWKey != "" && cfg.PDWAccount != "" {
		voice := &voicememo.Service{DB: cfg.DB, Engine: a.wf, URL: cfg.PDWURL, Key: cfg.PDWKey, Username: cfg.PDWAccount}
		go voice.Run(cfg.Context)
	}
	if cfg.Google != nil && cfg.EmailKey != nil {
		a.mail = &email.Service{DB: cfg.DB, Engine: a.wf, Google: cfg.Google, Key: cfg.EmailKey, Now: cfg.Now}
		interval := cfg.EmailInterval
		if interval == 0 {
			interval = email.Interval
		}
		go a.mail.Run(cfg.Context, interval)
	}
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
	mux.HandleFunc("POST /api/mcp-tokens", a.write(a.createMCPToken))
	mux.HandleFunc("DELETE /api/mcp-tokens/{id}", a.write(a.revokeMCPToken))
	mux.HandleFunc("/mcp", a.mcp)
	mux.HandleFunc("/mcp/{$}", a.mcp)
	mux.HandleFunc("GET /login", loginPage(cfg))
	mux.HandleFunc("GET /ui.css", stylesheet)
	mux.HandleFunc("GET /dictation.js", dictationScript)
	mux.HandleFunc("GET /attachments.js", attachmentsScript)
	mux.HandleFunc("POST /api/attachments", a.write(a.uploadAttachment))
	mux.HandleFunc("GET /submitkeys.js", submitKeysScript)
	mux.HandleFunc("GET /account", a.page(func(v viewer) http.HandlerFunc { return accountPage(cfg, v) }))
	// hosts: joining an account, and their streams
	mux.HandleFunc("POST /api/host-enrollments", a.enroll)
	mux.HandleFunc("POST /api/host-enrollments/{id}/key", a.collectKey)
	mux.HandleFunc("POST /api/host-enrollments/claim", a.write(a.claimHost))
	mux.HandleFunc("DELETE /api/hosts/{host}", a.write(a.removeHost))
	mux.HandleFunc("GET "+hostclient.StreamPath("{host}"), stream(a))
	mux.Handle("GET /api/host/binary/{os}/{arch}", binaries(cfg.BinDir))
	// before dev boxes were called hosts: a client of then updates itself from here, and old
	// links to its pages still land
	mux.Handle("GET /api/devbox/binary/{os}/{arch}", binaries(cfg.BinDir))
	mux.HandleFunc("GET /devboxes/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		u := *r.URL
		u.Path, u.RawPath = "/hosts/"+r.PathValue("rest"), ""
		http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
	})
	// what a signed-in account reads: its own hosts, through its own role
	mux.HandleFunc("GET /api/hosts", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			list, err := v.reg.List(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, list)
		}
	}))
	mux.HandleFunc("POST /api/hosts/{host}/commands", a.write(func(v viewer) http.HandlerFunc { return enqueueCommand(a, v) }))
	mux.HandleFunc("GET /api/hosts/{host}/commands/{id}", a.read(func(v viewer) http.HandlerFunc { return showCommand(a, v) }))
	mux.HandleFunc("POST "+dictationTokenPath, a.write(func(viewer) http.HandlerFunc { return dictationToken(cfg) }))
	mux.HandleFunc("GET /api/sql", a.read(func(v viewer) http.HandlerFunc { return sqlHandler(cfg, v.pool) }))
	mux.HandleFunc("POST /api/sql", a.read(func(v viewer) http.HandlerFunc { return sqlHandler(cfg, v.pool) }))
	mux.HandleFunc("GET /api/hosts/resources", a.read(func(v viewer) http.HandlerFunc { return hostResourcesAPI(v.reg) }))
	mux.HandleFunc("GET /api/usage", a.read(func(v viewer) http.HandlerFunc { return usageAPI(v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/workspaces/new", a.page(func(v viewer) http.HandlerFunc { return newWorkspacePage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/workspaces/{ws}/agents/new", a.page(func(v viewer) http.HandlerFunc { return newAgentPage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}", a.page(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptPage(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}/blocks", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptBlocks(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}/resources", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptResources(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}/entries/{seq}", a.page(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptEntry(v.reg, w, r) }
	}))
	mux.HandleFunc("GET /schedules", a.page(func(v viewer) http.HandlerFunc { return schedulesPage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/schedules/new", a.page(func(v viewer) http.HandlerFunc { return newSchedulePage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/schedules/{id}", a.page(func(v viewer) http.HandlerFunc { return schedulePage(cfg, v.reg) }))
	// workflows: written through the API, read through the account's role; a host's agents move runs with its key
	mux.HandleFunc("POST /api/workflows", a.write(a.createWorkflow))
	mux.HandleFunc("PUT /api/workflows/{id}", a.write(a.updateWorkflow))
	mux.HandleFunc("PUT /api/workflows/{id}/default", a.write(a.setDefaultWorkflow))
	mux.HandleFunc("DELETE /api/workflows/{id}", a.write(a.deleteWorkflow))
	mux.HandleFunc("POST /api/workflow-runs", a.write(a.startRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/reply", a.write(a.replyToRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/cancel", a.write(a.cancelRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/moves", a.moveRun)
	mux.HandleFunc("GET /workflows", a.page(func(v viewer) http.HandlerFunc { return workflowsPage(cfg, v) }))
	// email triggers: the account's Gmail mailbox, connected through Google's sign-in
	mux.HandleFunc("POST /api/gmail/sign-in", a.write(a.gmailSignIn))
	mux.HandleFunc("POST /api/gmail/connect", a.write(a.gmailConnect))
	mux.HandleFunc("PUT /api/gmail/owners", a.write(a.gmailOwners))
	mux.HandleFunc("DELETE /api/gmail", a.write(a.gmailDisconnect))
	mux.HandleFunc("GET /gmail/callback", gmailCallback)
	mux.HandleFunc("GET /workflows/email", a.page(func(v viewer) http.HandlerFunc { return emailPage(cfg, a, v) }))
	mux.HandleFunc("GET /workflows/{id}", a.page(func(v viewer) http.HandlerFunc { return workflowPage(cfg, a, v) }))
	mux.HandleFunc("GET /workflow-runs/{id}", a.page(func(v viewer) http.HandlerFunc { return runPage(cfg, v) }))
	mux.HandleFunc("GET /usage", a.page(func(v viewer) http.HandlerFunc { return usagePage(cfg, v.reg) }))
	mux.HandleFunc("GET /resources", a.page(func(v viewer) http.HandlerFunc { return hostResourcesPage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts", func(w http.ResponseWriter, r *http.Request) { // its name before the home page's hosts took it
		u := *r.URL
		u.Path, u.RawPath = "/resources", ""
		http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /{$}", a.page(func(v viewer) http.HandlerFunc { return homePage(cfg, v) }))
	// every response names the server's version, so a host sees on any call that it should update
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(hostclient.VersionHeader, cfg.Version)
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
		res, err := v.reg.HostResources(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]any{
			"Version":   cfg.Version,
			"Short":     short(cfg.Version),
			"Hosts":     list,
			"Plans":     plans,
			"Resources": res,
			"Now":       cfg.Now(),
			"Username":  v.acct.Username,
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

// humanDuration reads as 42s, 5m 0s or 1h 4m.
func humanDuration(d time.Duration) string {
	d = max(0, d).Truncate(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
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

// allAgents is every agent on host b, across its workspaces.
func allAgents(b fleet.Host) []fleet.AgentView {
	var out []fleet.AgentView
	for _, w := range b.Workspaces {
		out = append(out, w.Agents...)
	}
	return out
}

// idleWorkspaces are b's workspaces with no agents.
func idleWorkspaces(b fleet.Host) []fleet.WorkspaceAgents {
	var out []fleet.WorkspaceAgents
	for _, w := range b.Workspaces {
		if len(w.Agents) == 0 {
			out = append(out, w)
		}
	}
	return out
}

// openWorkspace says whether w starts open on the home page: it has an agent running or needing attention.
// runningAgents counts the agents working on a turn, which archiving would stop.
func runningAgents(agents []fleet.AgentView) int {
	n := 0
	for _, a := range agents {
		if working(a.Status) {
			n++
		}
	}
	return n
}

// losesChanges: archiving the workspace would remove a worktree holding uncommitted changes.
func losesChanges(w fleet.WorkspaceAgents) bool { return w.Kind == "worktree" && w.Dirty }

func openWorkspace(w fleet.WorkspaceAgents) bool {
	for _, a := range w.Agents {
		if a.Status == "running" || a.RequiresAttention {
			return true
		}
	}
	return false
}
