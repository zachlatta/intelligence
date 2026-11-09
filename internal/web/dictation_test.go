package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// fakeAssemblyAI is AssemblyAI's token endpoint: it checks the key and the request, and hands out tok.
func fakeAssemblyAI(t *testing.T, key, tok string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != key {
			http.Error(w, `{"error":"Invalid API key"}`, http.StatusUnauthorized)
			return
		}
		var body struct {
			Product    string `json:"product"`
			Expires    int    `json:"expires_in_seconds"`
			MaxSession int    `json:"max_session_duration_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Product != "streaming" ||
			body.Expires < 1 || body.Expires > 600 || body.MaxSession < 60 || body.MaxSession > 10800 {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_in_seconds": body.Expires})
	}))
	t.Cleanup(s.Close)
	return s
}

func dictationServer(t *testing.T, key, tokenURL string) http.Handler {
	t.Helper()
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.AssemblyAIKey = key; c.AssemblyAITokenURL = tokenURL })
	sendReport(t, h, sampleReport)
	return h
}

func TestDictationTokenIsMintedWithTheServersKey(t *testing.T) {
	aai := fakeAssemblyAI(t, "test-key", "temp-token")
	h := dictationServer(t, "test-key", aai.URL)

	rec := post(t, h, "/api/dictation/token", "", []byte(`{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct{ Token, URL string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Token != "temp-token" {
		t.Errorf("token = %q", got.Token)
	}
	if !strings.HasPrefix(got.URL, "wss://streaming.assemblyai.com/v3/ws?") || !strings.Contains(got.URL, "speech_model="+dictationModel) {
		t.Errorf("url = %q", got.URL)
	}
	if strings.Contains(rec.Body.String(), "test-key") {
		t.Error("the response carries the server's key")
	}

	if rec := post(t, anon(h), "/api/dictation/token", "", []byte(`{}`)); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/dictation/token", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://elsewhere.example")
	signedIn(h, req)
	other := httptest.NewRecorder()
	h.ServeHTTP(other, req)
	if other.Code != http.StatusForbidden {
		t.Errorf("another site: %d, want 403", other.Code)
	}
}

func TestDictationTokenFailsClearly(t *testing.T) {
	aai := fakeAssemblyAI(t, "right-key", "temp-token")
	rec := post(t, dictationServer(t, "wrong-key", aai.URL), "/api/dictation/token", "", []byte(`{}`))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Invalid API key") {
		t.Errorf("a key AssemblyAI refuses: %d %s, want 502 with its reason", rec.Code, rec.Body)
	}
	rec = post(t, dictationServer(t, "", aai.URL), "/api/dictation/token", "", []byte(`{}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no key: %d, want 503", rec.Code)
	}
}

func TestChatBoxOffersDictationOnlyWithAKey(t *testing.T) {
	aai := fakeAssemblyAI(t, "k", "t")
	page := get(t, dictationServer(t, "k", aai.URL), "/hosts/host-1/agents/"+agentA1, nil).Body.String()
	for _, want := range []string{`data-dictation="/api/dictation/token"`, `id="dictate"`, `src="/dictation.js"`} {
		if !strings.Contains(page, want) {
			t.Errorf("with a key, the page misses %s", want)
		}
	}
	page = get(t, dictationServer(t, "", aai.URL), "/hosts/host-1/agents/"+agentA1, nil).Body.String()
	if strings.Contains(page, `id="dictate"`) {
		t.Error("without a key, the page offers dictation")
	}
}

func TestSpawnPagesOfferDictationOnlyWithAKey(t *testing.T) {
	aai := fakeAssemblyAI(t, "k", "t")
	for _, p := range []string{newAgentPath, "/hosts/host-1/workspaces/new"} {
		page := get(t, dictationServer(t, "k", aai.URL), p, nil).Body.String()
		for _, want := range []string{`data-dictation="/api/dictation/token"`, `id="dictate"`, `src="/dictation.js"`} {
			if !strings.Contains(page, want) {
				t.Errorf("%s with a key misses %s", p, want)
			}
		}
		page = get(t, dictationServer(t, "", aai.URL), p, nil).Body.String()
		if strings.Contains(page, `id="dictate"`) || strings.Contains(page, `/dictation.js`) {
			t.Errorf("%s without a key offers dictation", p)
		}
	}
}

func TestDictationScriptIsServed(t *testing.T) {
	rec := get(t, dictationServer(t, "", ""), "/dictation.js", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Body.String(), "AudioWorklet") || rec.Header().Get("ETag") == "" {
		t.Errorf("/dictation.js: %d %q, etag %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("ETag"))
	}
}

func TestWorkflowRunOffersDictationOnlyWithAKey(t *testing.T) {
	for _, key := range []string{"k", ""} {
		t.Run("key="+key, func(t *testing.T) {
			h := dictationServer(t, key, "")
			id := createWorkflow(t, h, workflow.Starting)
			rec := get(t, h, "/workflows/"+id, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("workflow page: %d %s", rec.Code, rec.Body)
			}
			page := rec.Body.String()
			if key == "" {
				if strings.Contains(page, `id="dictate"`) || strings.Contains(page, `/dictation.js`) {
					t.Error("without a key, the workflow run box offers dictation")
				}
				return
			}
			for _, want := range []string{`data-dictation="/api/dictation/token"`, `id="dictate"`, `src="/dictation.js"`, `text: document.getElementById("s-input")`, `send: () => form.requestSubmit(start)`, `active: () => dlg.open`,
				`onstart: clearWarning`, `dlg.addEventListener("toggle", clearWarning)`,
				`error.textContent = ""; error.hidden = true;`, `#start .dictation { margin-top: 1.4rem; }`} {
				if !strings.Contains(page, want) {
					t.Errorf("workflow run box misses %s", want)
				}
			}
		})
	}
}

// TestEveryTaskEntryPointOffersDictation: every form that creates a task or starts agent work (a
// workflow task, a workflow run, a new agent, a new workspace, a new schedule) offers the shared
// dictation button, loads the engine and wires it to that form's own text box; none does without a key.
func TestEveryTaskEntryPointOffersDictation(t *testing.T) {
	entries := []struct {
		name, path string
		wiring     []string // the box the button dictates into, and how it sends
	}{
		{"workflow task", "/workflows", []string{`<textarea id="task"`, `text: document.getElementById("task")`,
			`send: () => submit.click()`, `#task-dictation .dictation`, `.composer .task-actions[hidden] { display: none; }`}},
		{"workflow run", "/workflows/{id}", []string{`<textarea id="s-input"`, `text: document.getElementById("s-input")`}},
		{"new agent", newAgentPath, []string{`id="spawn"`, `text: form.prompt`}},
		{"new workspace", "/hosts/host-1/workspaces/new", []string{`id="spawn"`, `text: form.prompt`}},
		{"new schedule", "/hosts/host-1/schedules/new", []string{`id="schedule"`, `text: form.prompt`,
			`send: () => form.requestSubmit()`}},
	}
	for _, key := range []string{"k", ""} {
		h := dictationServer(t, key, "")
		id := createWorkflow(t, h, workflow.Starting)
		for _, e := range entries {
			rec := get(t, h, strings.Replace(e.path, "{id}", id, 1), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", e.name, rec.Code, rec.Body)
			}
			page := rec.Body.String()
			if key == "" {
				if strings.Contains(page, `id="dictate"`) || strings.Contains(page, `/dictation.js`) {
					t.Errorf("%s without a key offers dictation", e.name)
				}
				continue
			}
			want := append([]string{`data-dictation="/api/dictation/token"`, `id="dictate"`, `src="/dictation.js"`,
				`mic: document.getElementById("dictate")`}, e.wiring...)
			for _, w := range want {
				if !strings.Contains(page, w) {
					t.Errorf("%s misses %s", e.name, w)
				}
			}
			if n := strings.Count(page, `id="dictate"`); n != 1 {
				t.Errorf("%s has %d dictation buttons, want 1", e.name, n)
			}
		}
	}
}
