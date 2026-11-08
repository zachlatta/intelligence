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

	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/fleet"
)

// maxTranscriptBatch bounds one transcript upload; dev boxes send at most a few MB at a time.
const maxTranscriptBatch = 32 << 20

func storeTranscript(cfg Config, reg *fleet.Registry, w http.ResponseWriter, r *http.Request) {
	if !devboxAuthorized(cfg, w, r) {
		return
	}
	var b devbox.TranscriptBatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTranscriptBatch)).Decode(&b); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "transcript batch too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad transcript batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !devbox.ValidName(b.Box) || b.AgentID == "" || b.Epoch == "" {
		http.Error(w, "bad transcript batch: box, agentId and epoch are required", http.StatusBadRequest)
		return
	}
	switch err := reg.StoreTranscript(r.Context(), b, cfg.Now()); {
	case errors.Is(err, fleet.ErrEpochChanged):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, fleet.ErrUnknownAgent):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, "could not store the transcript: "+err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

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
}).Parse(transcriptHTML + blocksHTML))

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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = transcriptTmpl.Execute(w, map[string]any{
		"Box": box, "Agent": agent, "Workspace": ws, "Siblings": siblings, "Transcript": tr, "Synced": err == nil,
		"Now": cfg.Now(), "Base": base, "Stats": stats, "Live": err == nil && live(agent), "Blocks": blocks,
		"BlocksData": blocksData{Base: base, Blocks: blocks, Now: cfg.Now()},
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
<style>:root{color-scheme:light dark;font-family:system-ui,sans-serif}body{max-width:60rem;margin:1.5rem auto;padding:0 1rem}
pre{white-space:pre-wrap;overflow-wrap:anywhere;font-size:.8rem;background:#8881;padding:.75rem;border-radius:.5rem}</style></head>
<body><p><a href="{{.Back}}">← Back to the transcript</a></p>
<h1 style="font-size:1.1rem">{{.Kind}}{{if .Tool}} · {{.Tool}}{{end}} <small style="opacity:.6">seq {{.Seq}}</small></h1>
{{range .Texts}}<h2 style="font-size:.9rem;opacity:.7">{{.Name}}</h2><pre>{{.Text}}</pre>{{end}}
<h2 style="font-size:.9rem;opacity:.7">Item</h2><pre>{{.Item}}</pre></body></html>`))

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
