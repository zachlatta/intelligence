package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/voicememo"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestVoiceMemoAPIStartsOnceAndIsAccountScoped(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v")
	sendReport(t, h, wfReport())
	def := workflow.Starting
	def.Trigger = workflow.TriggerVoiceMemo
	def.VoiceMemo = &workflow.VoiceMemoFilter{Title: "Ideas"}
	wf := createWorkflow(t, h, def)
	if page := get(t, h, "/workflows", nil); !strings.Contains(page.Body.String(), ">Voice memo</span>") || !strings.Contains(page.Body.String(), "title contains “Ideas”") {
		t.Fatal("list lacks voice memo label/filter")
	}
	if page := get(t, h, "/workflows/"+wf, nil); !strings.Contains(page.Body.String(), "Voice memo triggers are off on this server") {
		t.Fatal("editor lacks disabled status")
	}
	m := workflow.VoiceMemo{Source: "apple_voice_memos", Account: "owner", ID: "memo-1", Title: "Ideas", RecordedAt: "2026-11-11T12:00:00Z", Transcript: strings.Repeat("x", 1000) + "secret-tail", TranscriptLength: 1011}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next, more, row := "Mw", false, m
		if m.ID == "memo-1" {
			if r.URL.Query().Get("since") == "" {
				next, more = "MQ", true
			} else {
				next = "Mg"
				row.ID = "memo-2"
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"feed": "feed:voice-memos", "next_cursor": next, "has_more": more, "recordings": []workflow.VoiceMemo{row}})
	}))
	defer srv.Close()
	var username string
	if err := d.Owner.QueryRow(context.Background(), "SELECT username FROM accounts LIMIT 1").Scan(&username); err != nil {
		t.Fatal(err)
	}
	s := voicememo.Service{DB: d.Owner, Engine: workflow.NewEngine(d.Owner, nil), Username: username, URL: srv.URL, Key: "test"}
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, baseline := sqlPost(t, h, q("SELECT * FROM workflow_runs"))
	if len(baseline.Rows) != 0 {
		t.Fatal("initial backlog started a run")
	}
	if page := get(t, h, "/workflows/voice-memos", nil); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Baseline backlog skipped") {
		t.Fatal("missing baseline history: ", page.Code, page.Body)
	}
	m.ID = "new-memo"
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Poll(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	_, rows := sqlPost(t, h, q("SELECT decision,workflow_id::text,run_id::text,preview FROM voice_memos WHERE recording_id = 'new-memo'"))
	if len(rows.Rows) != 1 || rows.Rows[0][0] != "started" || rows.Rows[0][1] != wf {
		_, state := sqlPost(t, h, q("SELECT error FROM voice_memo_polling"))
		t.Fatalf("receipts: %+v; state: %+v", rows, state)
	}
	_, runs := sqlPost(t, h, q("SELECT input FROM workflow_runs"))
	if len(runs.Rows) != 1 || strings.Contains(runs.Rows[0][0].(string), "secret-tail") {
		t.Fatalf("runs: %+v", runs)
	}
	runID := rows.Rows[0][2].(string)
	if page := get(t, h, "/workflow-runs/"+runID, nil); !strings.Contains(page.Body.String(), "Voice memo that started this run") || !strings.Contains(page.Body.String(), "new-memo") || !strings.Contains(page.Body.String(), "Nov 11, 2026") {
		t.Fatal("run lacks memo attribution")
	}
	m.ID, m.Title = "unmatched", "Something else"
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if page := get(t, h, "/workflows/voice-memos", nil); !strings.Contains(page.Body.String(), "No workflow matched") || !strings.Contains(page.Body.String(), "Started run "+runID) {
		t.Fatal("history lacks decisions")
	}
	// A failed page must leave the cursor retryable and expose the failure through SQL.
	s.URL = "http://invalid.invalid"
	s.Client = &http.Client{Transport: voiceMemoFailTransport{}}
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, state := sqlPost(t, h, q("SELECT cursor,error FROM voice_memo_polling"))
	if len(state.Rows) != 1 || state.Rows[0][0] != "Mw" || state.Rows[0][1] == "" {
		t.Fatalf("poll state: %+v", state)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	configured := newServer(t, d, d.Owner, "v", func(c *Config) {
		c.Context = ctx
		c.PDWURL = "https://pdw.example.com"
		c.PDWKey = "test"
		c.PDWAccount = username
	})
	if page := get(t, configured, "/workflows/"+wf, nil); !strings.Contains(page.Body.String(), "PDW is configured for this account") || !strings.Contains(page.Body.String(), "PDW request failed") || !strings.Contains(page.Body.String(), "Last check:") {
		t.Fatal("missing polling status")
	}
	bob, err := h.store.Create(t.Context(), "other-voice", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, http.MethodPost, "/api/sql", q("SELECT * FROM voice_memos WHERE recording_id = 'new-memo'"), sameOriginFetch, sessionFor(t, h.store, bob))
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Fatal(rec.Body.String())
	}
	rec = call(t, h, http.MethodGet, "/workflows/voice-memos", "", nil, sessionFor(t, h.store, bob))
	if strings.Contains(rec.Body.String(), "new-memo") {
		t.Fatal("memo history leaked to another account")
	}
	rec = call(t, configured, http.MethodGet, "/workflows/voice-memos", "", nil, sessionFor(t, h.store, bob))
	if !strings.Contains(rec.Body.String(), "PDW is not configured for this account") || strings.Contains(rec.Body.String(), "PDW request failed") {
		t.Fatal("incorrect status for another account")
	}
	s.Username = "missing-account"
	if err := s.Poll(t.Context()); err == nil || !strings.Contains(err.Error(), `PDW_INTELLIGENCE_ACCOUNT "missing-account" does not exist`) {
		t.Fatalf("unclear missing account error: %v", err)
	}
	if page := get(t, h, "/workflows/"+wf, nil); !strings.Contains(page.Body.String(), "A voice memo is transcribed in PDW") {
		t.Fatal("missing trigger editor")
	}
}

type voiceMemoFailTransport struct{}

func (voiceMemoFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}
