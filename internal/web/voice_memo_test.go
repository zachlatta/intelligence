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
	_, runs := sqlPost(t, h, q("SELECT input, requested_at = TIMESTAMPTZ '2026-11-11T12:00:00Z' FROM workflow_runs"))
	if len(runs.Rows) != 1 || runs.Rows[0][1] != true || strings.Contains(runs.Rows[0][0].(string), "secret-tail") {
		t.Fatalf("runs: %+v", runs)
	}
	runID := rows.Rows[0][2].(string)
	if page := get(t, h, "/workflow-runs/"+runID, nil); !strings.Contains(page.Body.String(), "Voice memo that started this run") || !strings.Contains(page.Body.String(), "new-memo") || !strings.Contains(page.Body.String(), "Nov 11, 2026") {
		t.Fatal("run lacks memo attribution")
	}
	if page := get(t, h, "/workflow-runs/"+runID, nil); strings.Contains(page.Body.String(), `href="/workflow-runs/`+runID+`"`) || !strings.Contains(page.Body.String(), `<span class="grow"></span><a class="btn sm ghost" href="/workflows/voice-memos"`) {
		t.Fatal("run attribution links/layout are incorrect")
	}
	m.ID, m.Title = "unmatched", "Something else"
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if page := get(t, h, "/workflows/voice-memos", nil); !strings.Contains(page.Body.String(), "No workflow matched") || !strings.Contains(page.Body.String(), "Started run "+runID) {
		t.Fatal("history lacks decisions")
	}
	if page := get(t, h, "/workflows/voice-memos", nil); strings.Contains(page.Body.String(), `href="/workflows/voice-memos"`) || !strings.Contains(page.Body.String(), `href="/workflows/`+wf+`">Task from Intelligence</a>`) {
		t.Fatal("history has a self-link or lacks workflow attribution")
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

func TestVoiceMemoRefusalDoesNotBlockLaterRecordings(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreported host", true: "removed host"}[removed], func(t *testing.T) {
			d := dbtest.New(t)
			h := newServer(t, d, d.Owner, "v")
			sendReport(t, h, wfReport())
			joinHost(t, h.store, h.acct, "stale-host")
			stale := workflow.Starting
			stale.Host = "stale-host"
			stale.Trigger = workflow.TriggerVoiceMemo
			stale.VoiceMemo = &workflow.VoiceMemoFilter{Title: "Stale"}
			staleID := createWorkflow(t, h, stale)
			if removed {
				rec := call(t, h, http.MethodDelete, "/api/hosts/stale-host", "", sameOriginFetch)
				if rec.Code != http.StatusNoContent {
					t.Fatal(rec.Code, rec.Body)
				}
			}
			healthy := workflow.Starting
			healthy.Trigger = workflow.TriggerVoiceMemo
			healthy.VoiceMemo = &workflow.VoiceMemoFilter{Title: "Healthy"}
			healthyID := createWorkflow(t, h, healthy)
			memos := []workflow.VoiceMemo{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next := "MA"
				if len(memos) > 0 {
					next = "Mg"
				}
				json.NewEncoder(w).Encode(map[string]any{"feed": "feed:voice-memos", "next_cursor": next, "recordings": memos, "has_more": false})
			}))
			defer srv.Close()
			s := voicememo.Service{DB: d.Owner, Engine: workflow.NewEngine(d.Owner, nil), URL: srv.URL, Key: "test", Username: h.acct.Username}
			if err := s.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			memos = []workflow.VoiceMemo{{Source: "apple_voice_memos", Account: "owner", ID: "stale-memo", Title: "Stale", Transcript: "first"}, {Source: "apple_voice_memos", Account: "owner", ID: "healthy-memo", Title: "Healthy", Transcript: "second"}}
			for i := 0; i < 2; i++ {
				if err := s.Poll(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			_, receipts := sqlPost(t, h, q("SELECT recording_id,decision,reason,workflow_id::text,coalesce(run_id::text,'') FROM voice_memos ORDER BY recording_id DESC"))
			if len(receipts.Rows) != 2 {
				_, state := sqlPost(t, h, q("SELECT cursor,error FROM voice_memo_polling"))
				t.Fatalf("receipts: %+v; poll: %+v", receipts, state)
			}
			refusal := "has not reported yet"
			if removed {
				refusal = "not one of this account"
			}
			if receipts.Rows[0][1] != "ignored" || receipts.Rows[0][3] != staleID || receipts.Rows[0][4] != "" || !strings.Contains(receipts.Rows[0][2].(string), "could not start “Task from Intelligence”") || !strings.Contains(receipts.Rows[0][2].(string), refusal) {
				t.Fatalf("refusal receipt: %+v", receipts.Rows[0])
			}
			if receipts.Rows[1][1] != "started" || receipts.Rows[1][3] != healthyID || receipts.Rows[1][4] == "" {
				t.Fatalf("healthy receipt: %+v", receipts.Rows[1])
			}
			_, runs := sqlPost(t, h, q("SELECT workflow_id::text FROM workflow_runs"))
			if len(runs.Rows) != 1 || runs.Rows[0][0] != healthyID {
				t.Fatalf("refused run leaked: %+v", runs)
			}
			_, state := sqlPost(t, h, q("SELECT cursor,error FROM voice_memo_polling"))
			if state.Rows[0][0] != "Mg" || state.Rows[0][1] != "" {
				t.Fatalf("poll wedged: %+v", state)
			}
		})
	}
}

type voiceMemoFailTransport struct{}

func TestProposalsShowVoiceMemoFilterChanges(t *testing.T) {
	for _, filters := range [][2]*workflow.VoiceMemoFilter{
		{nil, {Title: "Ideas"}},
		{{Title: "Ideas"}, {Title: "Projects", Words: "workshop", Without: "private"}},
		{{Title: "Ideas"}, nil},
	} {
		before := &workflow.WorkflowState{Definition: workflow.Definition{VoiceMemo: filters[0]}}
		after := &workflow.WorkflowState{Definition: workflow.Definition{VoiceMemo: filters[1]}}
		changes := proposalChanges(before, after)
		if len(changes) != 1 || changes[0].Label != "Voice memo filter" || changes[0].Before != prettyProposal(filters[0]) || changes[0].After != prettyProposal(filters[1]) {
			t.Fatalf("proposal hides voice memo filter changes: %+v", changes)
		}
	}
}

func TestVoiceMemoRunsUseTheHostCapacityQueue(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v")
	sendReport(t, h, wfReport())
	lastMinute(t, d, 0, 99)
	def := workflow.Starting
	def.Priority = workflow.PriorityNormal
	def.Trigger = workflow.TriggerVoiceMemo
	def.VoiceMemo = &workflow.VoiceMemoFilter{}
	createWorkflow(t, h, def)
	memos := []workflow.VoiceMemo{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"feed": "feed:voice-memos", "next_cursor": "MQ", "recordings": memos, "has_more": false})
	}))
	defer srv.Close()
	engine := workflow.NewEngine(d.Owner, nil)
	s := voicememo.Service{DB: d.Owner, Engine: engine, URL: srv.URL, Key: "test", Username: h.acct.Username}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	memos = []workflow.VoiceMemo{{Source: "apple_voice_memos", Account: "demo", ID: "capacity-memo", Title: "Ideas", Transcript: "Build a workshop"}}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, rows := sqlPost(t, h, q("SELECT m.decision,r.status FROM voice_memos m JOIN workflow_runs r ON r.id=m.run_id"))
	if len(rows.Rows) != 1 || rows.Rows[0][0] != "started" || rows.Rows[0][1] != workflow.Queued {
		t.Fatalf("voice memo bypassed queue: %+v", rows)
	}
	if _, err := d.Owner.Exec(t.Context(), "UPDATE host_minutes SET cpu_avg_pct=0,load1=0,mem_available_min=100 WHERE host='host-1'"); err != nil {
		t.Fatal(err)
	}
	if err := engine.StartQueued(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, rows = sqlPost(t, h, q("SELECT status FROM workflow_runs"))
	if len(rows.Rows) != 1 || rows.Rows[0][0] != workflow.FindingProject {
		t.Fatalf("queued memo did not launch once: %+v", rows)
	}
}

// A recording PDW sends with no transcript yet must not wedge the feed: it gets an ignored
// receipt, and the recordings after it are still read.
func TestVoiceMemoEmptyTranscriptIsIgnoredNotRetried(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v")
	sendReport(t, h, wfReport())
	def := workflow.Starting
	def.Trigger = workflow.TriggerVoiceMemo
	def.VoiceMemo = &workflow.VoiceMemoFilter{}
	wf := createWorkflow(t, h, def)
	memos := []workflow.VoiceMemo{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := "MA"
		if len(memos) > 0 {
			next = "MQ"
		}
		json.NewEncoder(w).Encode(map[string]any{"feed": "feed:voice-memos", "next_cursor": next, "recordings": memos, "has_more": false})
	}))
	defer srv.Close()
	s := voicememo.Service{DB: d.Owner, Engine: workflow.NewEngine(d.Owner, nil), URL: srv.URL, Key: "test", Username: h.acct.Username}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	memos = []workflow.VoiceMemo{{Source: "apple_voice_memos", Account: "owner", ID: "blank", Title: "Blank", Transcript: " \n "},
		{Source: "apple_voice_memos", Account: "owner", ID: "spoken", Title: "Spoken", Transcript: "hello"}}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, state := sqlPost(t, h, q("SELECT cursor,error FROM voice_memo_polling"))
	if state.Rows[0][0] != "MQ" || state.Rows[0][1] != "" {
		t.Fatalf("poll wedged: %+v", state)
	}
	_, receipts := sqlPost(t, h, q("SELECT recording_id,decision,reason,coalesce(workflow_id::text,'') FROM voice_memos ORDER BY recording_id"))
	if len(receipts.Rows) != 2 || receipts.Rows[0][1] != "ignored" || receipts.Rows[0][2] != "No transcript" || receipts.Rows[0][3] != "" ||
		receipts.Rows[1][1] != "started" || receipts.Rows[1][3] != wf {
		t.Fatalf("receipts: %+v", receipts)
	}
}

func TestVoiceMemoUnexpectedStartFailureRemainsRetryable(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v")
	sendReport(t, h, wfReport())
	def := workflow.Starting
	def.Trigger = workflow.TriggerVoiceMemo
	def.VoiceMemo = &workflow.VoiceMemoFilter{}
	createWorkflow(t, h, def)
	memos := []workflow.VoiceMemo{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := "MA"
		if len(memos) > 0 {
			next = "MQ"
		}
		json.NewEncoder(w).Encode(map[string]any{"feed": "feed:voice-memos", "next_cursor": next, "recordings": memos, "has_more": false})
	}))
	defer srv.Close()
	s := voicememo.Service{DB: d.Owner, Engine: workflow.NewEngine(d.Owner, nil), URL: srv.URL, Key: "test", Username: h.acct.Username}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Fail after the run insert, so both the partial run and its receipt must be rolled back.
	_, err := d.Owner.Exec(t.Context(), `CREATE FUNCTION reject_voice_run() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'simulated start failure'; END $$;
 CREATE TRIGGER reject_voice_run BEFORE INSERT ON workflow_run_events FOR EACH ROW EXECUTE FUNCTION reject_voice_run()`)
	if err != nil {
		t.Fatal(err)
	}
	memos = []workflow.VoiceMemo{{Source: "apple_voice_memos", Account: "owner", ID: "retry-memo", Title: "Retry", Transcript: "hello"}}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"voice_memos", "workflow_runs"} {
		_, rows := sqlPost(t, h, q("SELECT * FROM "+table))
		if len(rows.Rows) != 0 {
			t.Fatalf("partial %s leaked: %+v", table, rows)
		}
	}
	_, state := sqlPost(t, h, q("SELECT cursor,error FROM voice_memo_polling"))
	if state.Rows[0][0] != "MA" || !strings.Contains(state.Rows[0][1].(string), "simulated start failure") {
		t.Fatalf("error lost: %+v", state)
	}
	if _, err := d.Owner.Exec(t.Context(), "DROP TRIGGER reject_voice_run ON workflow_run_events; DROP FUNCTION reject_voice_run()"); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, receipts := sqlPost(t, h, q("SELECT decision FROM voice_memos"))
	if len(receipts.Rows) != 1 || receipts.Rows[0][0] != "started" {
		t.Fatalf("retry failed: %+v", receipts)
	}
	_, state = sqlPost(t, h, q("SELECT cursor,error FROM voice_memo_polling"))
	if state.Rows[0][0] != "MQ" || state.Rows[0][1] != "" {
		t.Fatalf("cursor not advanced: %+v", state)
	}
}

func (voiceMemoFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}
