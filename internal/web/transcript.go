package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
)

//go:embed transcript.html
var transcriptHTML string

//go:embed blocks.html
var blocksHTML string

// pairOf carries two values into a nested template.
type pairOf struct{ A, B any }

var transcriptTmpl = template.Must(template.New("transcript").Funcs(template.FuncMap{
	"ago":      ago,
	"plural":   plural,
	"shortID":  func(id string) string { return id[:min(7, len(id))] },
	"pair":     func(a, b any) pairOf { return pairOf{a, b} },
	"md":       markdown,
	"preview":  preview,
	"noteText": noteText,
	"iso":      func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"deref":    func(p *int) int { return *p },
	"dur":      dur,
	"size":     size,
	"plural64": func(n int64, word string) string { return plural(int(n), word) },
}).Parse(transcriptHTML + blocksHTML + resourcesHTML + layoutHTML))

// blocksTmpl renders blocks alone, for live updates.
var blocksTmpl = transcriptTmpl

// agentContext finds an agent on box, its workspace and the other agents in that workspace.
func agentContext(r *http.Request, reg *fleet.Registry, box, agentID string) (*fleet.AgentView, *fleet.WorkspaceAgents, []fleet.AgentView, error) {
	boxes, err := reg.List(r.Context())
	if err != nil {
		return nil, nil, nil, err
	}
	for _, b := range boxes {
		if b.Report.Name != box {
			continue
		}
		for i := range b.Workspaces {
			ws := &b.Workspaces[i]
			for j := range ws.Agents {
				if ws.Agents[j].ID != agentID {
					continue
				}
				var sib []fleet.AgentView
				for _, a := range ws.Agents {
					if a.ID != agentID {
						sib = append(sib, a)
					}
				}
				return &ws.Agents[j], ws, sib, nil
			}
		}
	}
	return nil, nil, nil, nil
}

// live: the agent is loaded in Paseo, so its transcript may still grow.
func live(a *fleet.AgentView) bool { return a.Status != "closed" }

type transcriptStats struct{ Entries, Turns, Tools int }

// transcriptPage shows one agent's transcript as turns of messages, thinking and tool calls.
func transcriptPage(cfg Config, reg *fleet.Registry, w http.ResponseWriter, r *http.Request) {
	box, agentID := r.PathValue("box"), r.PathValue("agent")
	agent, ws, siblings, err := agentContext(r, reg, box, agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agent == nil {
		http.NotFound(w, r)
		return
	}
	tr, err := reg.Transcript(r.Context(), box, agentID, 0)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats := transcriptStats{Entries: len(tr.Entries)}
	for _, e := range tr.Entries {
		switch e.Kind {
		case "user_message":
			stats.Turns++
		case "tool_call":
			stats.Tools++
		}
	}
	base := "/devboxes/" + box + "/agents/" + agentID
	blocks := buildBlocks(tr.Entries)
	res, rerr := agentUse(r, cfg, reg, box, agentID, live(agent))
	if rerr != nil {
		http.Error(w, rerr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = transcriptTmpl.Execute(w, map[string]any{
		"Box": box, "Agent": agent, "Workspace": ws, "Siblings": siblings, "Transcript": tr, "Synced": err == nil,
		"Now": cfg.Now(), "Base": base, "Stats": stats, "Live": err == nil && live(agent), "Blocks": blocks,
		"BlocksData": blocksData{Base: base, Blocks: blocks, Now: cfg.Now()}, "Resources": res,
		"Chat": map[string]string{"Commands": "/api/devboxes/" + box + "/commands", "PendingSQL": pendingMessagesSQL(box, agentID),
			"QueueTTL": queuedMessageTTL},
	})
}

// transcriptBlocks sends the blocks from seq `from` on, for a live page to replace its tail with.
func transcriptBlocks(cfg Config, reg *fleet.Registry, w http.ResponseWriter, r *http.Request) {
	box, agentID := r.PathValue("box"), r.PathValue("agent")
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	agent, _, _, err := agentContext(r, reg, box, agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tr, err := reg.Transcript(r.Context(), box, agentID, from)
	if agent == nil || errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Epoch", tr.Epoch)
	w.Header().Set("X-Agent-Status", agent.Status)
	_ = blocksTmpl.ExecuteTemplate(w, "blocks", blocksData{Base: "/devboxes/" + box + "/agents/" + agentID,
		Blocks: buildBlocks(tr.Entries), Now: cfg.Now()})
}

var entryTmpl = template.Must(template.New("entry").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Entry {{.Seq}} · Intelligence</title>
{{template "css"}}
<style>pre { white-space: pre-wrap; overflow-wrap: anywhere; margin: 0; padding: .9rem 1rem; font-size: 12.5px; }
.card-head .mono { font-size: 12.5px; }</style></head>
<body>{{template "top" "/"}}
<main class="container page">
<div class="crumbs"><a href="{{.Back}}">← Back to the transcript</a></div>
<div class="page-head"><h1>{{.Kind}}{{if .Tool}} · {{.Tool}}{{end}}</h1><span class="badge">seq {{.Seq}}</span></div>
{{range .Texts}}<section class="card"><div class="card-head"><h2>{{.Name}}</h2></div><pre>{{.Text}}</pre></section>{{end}}
<section class="card"><div class="card-head"><h2>Item</h2></div><pre>{{.Item}}</pre></section>
</main></body></html>` + layoutHTML))

// transcriptEntry shows one entry whole: its long texts as text, then its item as JSON.
func transcriptEntry(reg *fleet.Registry, w http.ResponseWriter, r *http.Request) {
	box, agentID := r.PathValue("box"), r.PathValue("agent")
	seq, err := strconv.Atoi(r.PathValue("seq"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	e, err := reg.TranscriptEntry(r.Context(), box, agentID, seq)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type named struct{ Name, Text string }
	var texts []named
	var it struct {
		Text   string `json:"text"`
		Detail map[string]any
	}
	_ = json.Unmarshal(e.Item, &it)
	if it.Text != "" {
		texts = append(texts, named{"Text", it.Text})
	}
	for _, k := range []string{"command", "output", "content", "unifiedDiff", "result", "log"} {
		if s, ok := it.Detail[k].(string); ok && s != "" {
			texts = append(texts, named{k, s})
		}
	}
	var item bytes.Buffer
	if json.Indent(&item, e.Item, "", "  ") != nil {
		item.Write(e.Item)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = entryTmpl.Execute(w, map[string]any{"Back": "/devboxes/" + box + "/agents/" + agentID + "#seq-" + strconv.Itoa(seq),
		"Kind": e.Kind, "Tool": e.Tool, "Seq": seq, "Texts": texts, "Item": item.String()})
}

// preview is the first line of a text, at most 160 characters.
func preview(s string) string {
	s = strings.TrimSpace(strings.TrimLeft(s, "#*_> "))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}

// noteText is a short line for an entry that is not a message, thinking or a tool call.
func noteText(e entryView) string {
	switch e.Kind {
	case "compaction":
		return "Context compacted"
	case "error":
		return "Error: " + e.Text
	case "turn_failed":
		return "Turn failed"
	case "turn_canceled":
		return "Turn canceled"
	case "todo":
		return "Updated the to-do list"
	}
	if e.Text != "" {
		return strings.ReplaceAll(e.Kind, "_", " ") + ": " + e.Text
	}
	return strings.ReplaceAll(e.Kind, "_", " ")
}
