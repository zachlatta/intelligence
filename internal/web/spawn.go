package web

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// The browser holds the operator's session in sessionCookie: a hash of the operator token, so the
// cookie is no use where the token is wanted, and changing the token signs every browser out.
const (
	sessionCookie = "intelligence_operator"
	sessionMaxAge = 30 * 24 * time.Hour
)

// commandProgressSQL is how the spawn pages follow their command, through the read-only SQL API
// ($ID is the command's id): its status and error, what it made, and whether the box has
// reported the agent yet (its transcript page has something to show).
const commandProgressSQL = `SELECT c.status, c.error, coalesce(c.result ->> 'workspaceId', '') AS workspace_id,
	coalesce(c.result ->> 'agentId', '') AS agent_id,
	EXISTS (SELECT 1 FROM agents a WHERE a.box = c.box AND a.id = c.result ->> 'agentId') AS reported
	FROM commands c WHERE c.id = '$ID'`

// newAgent is what the spawn forms start with: the fleet's default agent (Opus 5.5, medium
// thinking, no permission prompts, since nobody watches its terminal).
var newAgent = struct{ Provider, Model, Thinking, Mode string }{"claude", "claude-opus-5-5", "medium", "bypassPermissions"}

var thinkingLevels = []string{"low", "medium", "high", "xhigh", "max"}

//go:embed spawn.html
var spawnHTML string

var spawnTmpl = template.Must(template.New("spawn").Funcs(template.FuncMap{"short": short}).Parse(spawnHTML))

func sessionValue(token string) string {
	sum := sha256.Sum256([]byte("intelligence operator session\x00" + token))
	return hex.EncodeToString(sum[:])
}

// hasSession says whether r carries the operator's session cookie.
func hasSession(cfg Config, r *http.Request) bool {
	if cfg.OperatorToken == "" {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionValue(cfg.OperatorToken))) == 1
}

// sameOrigin refuses a browser request another site started. The cookie is SameSite=Strict as
// well; this also stops a sibling site (same-site, another origin). Sec-Fetch-Site, which the
// browser sets, decides when present: behind a proxy the Host seen here need not be the one the
// browser used. Without it, Origin (if any) must name this host.
func sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin" || s == "none"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host != "" && (u.Host == r.Host || u.Host == r.Header.Get("X-Forwarded-Host"))
	}
	return true
}

// operatorOrSession answers the request itself unless it carries the operator token, or is a
// same-origin JSON request from a signed-in browser. JSON is required of a browser because another
// site cannot send it without the server's leave.
func operatorOrSession(cfg Config, w http.ResponseWriter, r *http.Request) bool {
	if _, ok := bearerOf(r); ok || !hasSession(cfg, r) {
		return operatorAuthorized(cfg, w, r)
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return false
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// signIn is POST /api/session {"token"}: it signs a browser in with the operator token.
func signIn(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.OperatorToken == "" {
			http.Error(w, "this server has no operator token (OPERATOR_TOKEN)", http.StatusServiceUnavailable)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.Token), []byte(cfg.OperatorToken)) != 1 {
			http.Error(w, "that is not the operator token", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sessionValue(cfg.OperatorToken), Path: "/",
			MaxAge: int(sessionMaxAge.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	}
}

// signOut is DELETE /api/session.
func signOut(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// localPath is next if it is a path on this site, else "/".
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := spawnTmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func loginPage(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		render(w, "login", map[string]any{"Version": cfg.Version, "Next": localPath(r.URL.Query().Get("next")),
			"Configured": cfg.OperatorToken != ""})
	}
}

// needSession sends a browser with no session to sign in, and back here after.
func needSession(cfg Config, w http.ResponseWriter, r *http.Request) bool {
	if hasSession(cfg, r) {
		return true
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
	return false
}

// boxNamed is the box called name, or false.
func boxNamed(reg *fleet.Registry, r *http.Request, name string) (fleet.Box, bool, error) {
	list, err := reg.List(r.Context())
	for _, b := range list {
		if b.Report.Name == name {
			return b, true, err
		}
	}
	return fleet.Box{}, false, err
}

func availableProviders(b fleet.Box) []string {
	var out []string
	for _, p := range b.Report.Paseo.Providers {
		if p.Available {
			out = append(out, p.Name)
		}
	}
	return out
}

func spawnData(cfg Config, b fleet.Box) map[string]any {
	box := b.Report.Name
	return map[string]any{"Version": cfg.Version, "Box": box, "Providers": availableProviders(b), "Agent": newAgent,
		"Thinking": thinkingLevels, "Commands": "/api/devboxes/" + box + "/commands", "ProgressSQL": commandProgressSQL}
}

// newAgentPage is the form that starts an agent in a workspace.
func newAgentPage(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !needSession(cfg, w, r) {
			return
		}
		b, ok, err := boxNamed(reg, r, r.PathValue("box"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var ws *fleet.WorkspaceAgents
		for i := range b.Workspaces {
			if b.Workspaces[i].ID == r.PathValue("ws") && b.Workspaces[i].Listed {
				ws = &b.Workspaces[i]
			}
		}
		if !ok || ws == nil {
			http.NotFound(w, r)
			return
		}
		data := spawnData(cfg, b)
		data["Workspace"] = ws
		render(w, "agent", data)
	}
}

// repository is a repository a new workspace can be made from.
type repository struct{ Root, Project string }

// newWorkspacePage is the form that makes a workspace, a new worktree of one of the box's
// repositories by default, with an agent in it.
func newWorkspacePage(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !needSession(cfg, w, r) {
			return
		}
		b, ok, err := boxNamed(reg, r, r.PathValue("box"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		seen := map[string]bool{}
		var repos []repository
		for _, ws := range b.Workspaces { // most recently active first
			if ws.ProjectRoot != "" && !seen[ws.ProjectRoot] {
				seen[ws.ProjectRoot] = true
				repos = append(repos, repository{ws.ProjectRoot, ws.Project})
			}
		}
		data := spawnData(cfg, b)
		data["Repositories"] = repos
		render(w, "workspace", data)
	}
}
