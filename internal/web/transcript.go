package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"

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

var transcriptTmpl = template.Must(template.New("transcript").Funcs(template.FuncMap{
	"ago":     ago,
	"shortID": func(id string) string { return id[:min(7, len(id))] },
	"detail":  toolDetail,
}).Parse(transcriptHTML))

// transcriptPage shows one agent and its synced transcript, oldest first.
func transcriptPage(cfg Config, reg *fleet.Registry, w http.ResponseWriter, r *http.Request) {
	box, agentID := r.PathValue("box"), r.PathValue("agent")
	boxes, err := reg.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var agent *fleet.AgentView
	var ws *fleet.WorkspaceAgents
	for _, b := range boxes {
		if b.Report.Name != box {
			continue
		}
		for i := range b.Workspaces {
			for j := range b.Workspaces[i].Agents {
				if b.Workspaces[i].Agents[j].ID == agentID {
					ws, agent = &b.Workspaces[i], &b.Workspaces[i].Agents[j]
				}
			}
		}
	}
	if agent == nil {
		http.NotFound(w, r)
		return
	}
	tr, err := reg.Transcript(r.Context(), box, agentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = transcriptTmpl.Execute(w, map[string]any{
		"Box": box, "Agent": agent, "Workspace": ws, "Transcript": tr, "Synced": err == nil, "Now": cfg.Now(), "Version": cfg.Version,
	})
}

// toolDetail is a tool call's detail and error as indented JSON, cut to 20 KB for the page.
func toolDetail(item json.RawMessage) string {
	var v struct {
		Detail json.RawMessage `json:"detail"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(item, &v) != nil {
		return ""
	}
	out := map[string]json.RawMessage{"detail": v.Detail}
	if len(v.Error) > 0 && string(v.Error) != "null" {
		out["error"] = v.Error
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // the template escapes it for the page; \u003c would show as such
	enc.SetIndent("", "  ")
	if enc.Encode(out) != nil {
		return ""
	}
	b := bytes.TrimRight(buf.Bytes(), "\n")
	const max = 20 << 10
	if len(b) > max {
		return string(b[:max]) + "\n… (cut; the whole item is in transcript_entries.item)"
	}
	return string(b)
}
