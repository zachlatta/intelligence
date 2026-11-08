package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/devbox"
)

const agentA1 = "a1000000-0000-4000-8000-000000000001"

func batch(replace bool, epoch string, entries ...devbox.TranscriptEntry) devbox.TranscriptBatch {
	return devbox.TranscriptBatch{Box: "devbox-1", AgentID: agentA1, Epoch: epoch, Replace: replace, Entries: entries}
}

func tentry(start int, item string) devbox.TranscriptEntry {
	return devbox.TranscriptEntry{SeqStart: start, SeqEnd: start, Provider: "claude", At: t0.Add(time.Duration(start) * time.Minute), Item: json.RawMessage(item)}
}

func reported(t *testing.T) http.Handler {
	t.Helper()
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	return h
}

func TestTranscriptPageShowsEntriesEscaped(t *testing.T) {
	h := reported(t)
	sendBatch(t, h, batch(true, "ep1",
		tentry(1, `{"type":"user_message","text":"fix the <b>login</b>"}`),
		tentry(2, `{"type":"reasoning","text":"thinking hard"}`),
		tentry(3, `{"type":"tool_call","callId":"c","name":"Bash","status":"failed","detail":{"input":{"command":"ls <x>"}},"error":"exit 1"}`),
		tentry(4, `{"type":"assistant_message","text":"done"}`)))
	rec := get(t, h, "/devboxes/devbox-1/agents/"+agentA1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{"Fix the &lt;b&gt;login&lt;/b&gt; page", "fix the &lt;b&gt;login&lt;/b&gt;", "thinking hard",
		"Bash", "failed", "ls &lt;x&gt;", "done", "Login fix"} {
		if !strings.Contains(page, want) {
			t.Errorf("transcript page misses %q", want)
		}
	}
	if strings.Contains(page, "<b>login</b>") || strings.Contains(page, "ls <x>") {
		t.Errorf("transcript content must be escaped")
	}
	if strings.Index(page, "fix the &lt;b&gt;") > strings.Index(page, ">done<") {
		t.Errorf("entries should read oldest first")
	}
}

func TestTranscriptPageSaysWhyThereIsNone(t *testing.T) {
	h, _ := dbServer(t)
	rep := sampleReport
	rep.Paseo.Agents = append([]devbox.Agent(nil), rep.Paseo.Agents...)
	rep.Paseo.Agents[1].Status = "closed"
	sendReport(t, h, rep)
	rec := get(t, h, "/devboxes/devbox-1/agents/"+rep.Paseo.Agents[1].ID, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "closed before") {
		t.Errorf("a closed agent with no transcript should say why: %d", rec.Code)
	}
	if rec := get(t, h, "/devboxes/devbox-1/agents/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent: %d, want 404", rec.Code)
	}
}

func TestHomePageLinksAgentsToTranscripts(t *testing.T) {
	h := reported(t)
	sendBatch(t, h, batch(true, "ep1", tentry(1, `{"type":"user_message","text":"hi"}`)))
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, `href="/devboxes/devbox-1/agents/`+agentA1+`"`) {
		t.Errorf("home page should link each agent to its transcript")
	}
}

func TestTranscriptPageShowsWhenTheAgentIsWorking(t *testing.T) {
	h, _ := dbServer(t)
	rep := sampleReport
	rep.Paseo.Agents = append([]devbox.Agent(nil), rep.Paseo.Agents...)
	rep.Paseo.Agents[0].LastUserMessageAt = t0.Add(-3 * time.Minute)
	sendReport(t, h, rep)

	page := get(t, h, "/devboxes/devbox-1/agents/"+agentA1, nil).Body.String()
	for _, want := range []string{`<div id="working" class="working">`, `data-since="` + t0.Add(-3*time.Minute).UTC().Format(time.RFC3339) + `"`,
		`<title>● Fix the`, `class="spinner"`} {
		if !strings.Contains(page, want) {
			t.Errorf("a running agent's page misses %q", want)
		}
	}

	idle := get(t, h, "/devboxes/devbox-1/agents/"+rep.Paseo.Agents[1].ID, nil).Body.String()
	if !strings.Contains(idle, `<div id="working" class="working" hidden>`) || strings.Contains(idle, "<title>●") {
		t.Errorf("an idle agent's page should hide the working indicator")
	}
}
