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
	m := workflow.VoiceMemo{Source: "apple_voice_memos", Account: "owner", ID: "memo-1", Title: "Ideas", Transcript: strings.Repeat("x", 1000) + "secret-tail", TranscriptLength: 1011}
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
		t.Fatalf("receipts: %+v", rows)
	}
	_, runs := sqlPost(t, h, q("SELECT input FROM workflow_runs"))
	if len(runs.Rows) != 1 || strings.Contains(runs.Rows[0][0].(string), "secret-tail") {
		t.Fatalf("runs: %+v", runs)
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
	bob, err := h.store.Create(t.Context(), "other-voice", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, http.MethodPost, "/api/sql", q("SELECT * FROM voice_memos WHERE recording_id = 'new-memo'"), sameOriginFetch, sessionFor(t, h.store, bob))
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Fatal(rec.Body.String())
	}
	if page := get(t, h, "/workflows/"+wf, nil); !strings.Contains(page.Body.String(), "A voice memo is transcribed in PDW") {
		t.Fatal("missing trigger editor")
	}
}

type voiceMemoFailTransport struct{}

func (voiceMemoFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}
