package web

import (
	_ "embed"
	"html/template"
	"net/http"
	"strings"

	"github.com/zachlatta/intelligence/internal/fleet"
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

//go:embed account.html
var accountHTML string

var spawnTmpl = template.Must(template.New("spawn").Funcs(template.FuncMap{"short": short, "ago": ago}).
	Parse(spawnHTML + layoutHTML + accountHTML))

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
