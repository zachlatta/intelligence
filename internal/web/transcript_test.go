package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func postJSON(t *testing.T, h http.Handler, path, token string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(v)
	return post(t, h, path, token, b)
}

func reported(t *testing.T) http.Handler {
	t.Helper()
	h, _ := dbServer(t)
	if rec := post(t, h, devbox.ReportPath, "secret", reportJSON(t, sampleReport)); rec.Code != http.StatusNoContent {
		t.Fatalf("report: %d", rec.Code)
	}
	return h
}

func TestTranscriptUploadNeedsTheToken(t *testing.T) {
	h := reported(t)
	if rec := postJSON(t, h, devbox.TranscriptPath, "wrong", batch(true, "ep1", tentry(1, `{"type":"user_message","text":"hi"}`))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if rec := get(t, h, devbox.TranscriptCursorsPath("devbox-1"), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cursors without a token: %d, want 401", rec.Code)
	}
}

func TestTranscriptUploadAndCursors(t *testing.T) {
	h := reported(t)
	rec := postJSON(t, h, devbox.TranscriptPath, "secret", batch(true, "ep1",
		tentry(1, `{"type":"user_message","text":"fix the <b>login</b>"}`),
		tentry(2, `{"type":"assistant_message","text":"on it"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodGet, devbox.TranscriptCursorsPath("devbox-1"), nil)
	req.Header.Set("Authorization", "Bearer secret")
	cr := httptest.NewRecorder()
	h.ServeHTTP(cr, req)
	var cur map[string]devbox.TranscriptCursor
	if err := json.Unmarshal(cr.Body.Bytes(), &cur); err != nil || cur[agentA1].Epoch != "ep1" || cur[agentA1].LastSeqStart != 2 {
		t.Fatalf("cursors = %s (%v)", cr.Body, err)
	}
	if rec := postJSON(t, h, devbox.TranscriptPath, "secret", batch(false, "ep9", tentry(3, `{"type":"reasoning","text":"x"}`))); rec.Code != http.StatusConflict {
		t.Errorf("append from another epoch: %d, want 409", rec.Code)
	}
	ghost := batch(true, "ep1", tentry(1, `{"type":"user_message","text":"x"}`))
	ghost.AgentID = "ghost"
	if rec := postJSON(t, h, devbox.TranscriptPath, "secret", ghost); rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent: %d, want 404", rec.Code)
	}
}

func TestTranscriptPageShowsEntriesEscaped(t *testing.T) {
	h := reported(t)
	postJSON(t, h, devbox.TranscriptPath, "secret", batch(true, "ep1",
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
	post(t, h, devbox.ReportPath, "secret", reportJSON(t, rep))
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
	postJSON(t, h, devbox.TranscriptPath, "secret", batch(true, "ep1", tentry(1, `{"type":"user_message","text":"hi"}`)))
	page := get(t, h, "/", nil).Body.String()
	if !strings.Contains(page, `href="/devboxes/devbox-1/agents/`+agentA1+`"`) || !strings.Contains(page, "1 entry") {
		t.Errorf("home page should link each agent to its transcript with its size")
	}
}
