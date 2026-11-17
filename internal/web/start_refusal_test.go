package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/voicememo"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func refusalTestToken() string {
	return "pdw_" + strings.Repeat("a", 12) + "_" + strings.Repeat("x", 40)
}

func TestCredentialEmailIsIgnoredAndLaterMailRoutes(t *testing.T) {
	h, fake := emailServer(t)
	sendReport(t, h, wfReport())
	christina(t, h)
	connectGmail(t, h, fake)
	fake.Add(mailMsg("bad", "t-bad", "christina@example.com", "Q4", refusalTestToken()))
	fake.Add(mailMsg("good", "t-good", "christina@example.com", "Q4", "Prepare the deck"))
	bad := decided(t, h, "bad")
	if bad["decision"] != "ignored" || bad["run_id"] != "" || !strings.Contains(bad["reason"].(string), "credential") || strings.Contains(bad["reason"].(string), refusalTestToken()) {
		t.Fatal("credential email did not get a safe refusal")
	}
	if decided(t, h, "good")["decision"] != "started" {
		t.Fatal("later email did not route")
	}
}

func TestCredentialQueuedRunFailsAndNextRunStarts(t *testing.T) {
	for _, source := range []string{"legacy input", "project instructions", "orchestrator instructions"} {
		t.Run(source, func(t *testing.T) {
			db := dbtest.New(t)
			h := newServer(t, db, db.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
			sendReport(t, h, wfReport())
			b := openStream(t, h, "host-1")
			hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 97, 0.5)
			def := workflow.Starting
			if source != "project instructions" {
				def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			}
			badWF := createWorkflow(t, h, def)
			raw, _ := json.Marshal(map[string]string{"workflowId": badWF, "input": refusalTestToken()})
			if rec := call(t, h, http.MethodPost, "/api/workflow-runs", string(raw), sameOriginFetch); rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), refusalTestToken()) {
				t.Fatal("credential input was saved in the busy host queue")
			}
			first := startRun(t, h, badWF, "First task")
			second := startRun(t, h, fixedWorkflow(t, h), "Second task")
			if source == "legacy input" {
				if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_runs SET input=$2 WHERE id=$1", first, refusalTestToken()); err != nil {
					t.Fatal(err)
				}
			} else {
				field := "{orchestrator,instructions}"
				if source == "project instructions" {
					field = "{project,instructions}"
				}
				if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_versions SET definition=jsonb_set(definition,$2::text[],to_jsonb($3::text)) WHERE workflow_id=$1", badWF, field, refusalTestToken()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Owner.Exec(t.Context(), "DELETE FROM host_minutes"); err != nil {
				t.Fatal(err)
			}
			if err := workflow.NewEngine(db.Owner, nil).StartQueued(t.Context()); err != nil {
				t.Fatalf("queue retried a permanent refusal: %v", err)
			}
			if runField(t, h, first, "status") != "failed" || runField(t, h, second, "status") != "starting" {
				t.Fatal("refused run held up queue")
			}
			reason := runField(t, h, first, "error").(string)
			if !strings.Contains(reason, "credential") || strings.Contains(reason, refusalTestToken()) {
				t.Fatal("unsafe or missing failure reason")
			}
			if runField(t, h, first, "finished_at") == nil {
				t.Fatal("failed run has no finish time")
			}
		})
	}
}

func TestCredentialStartRetryFailsAndNextRunStarts(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "finder", true: "orchestrator"}[fixed], func(t *testing.T) {
			db := dbtest.New(t)
			h := newServer(t, db, db.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
			rep := wfReport()
			rep.StartRetryKeys = true
			b := sendReport(t, h, rep)
			def := workflow.Starting
			if fixed {
				def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
			}
			wf := createWorkflow(t, h, def)
			first := startRun(t, h, wf, "First task")
			original := b.command()
			hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 97, 0.5)
			second := startRun(t, h, fixedWorkflow(t, h), "Second task")
			if _, err := db.Owner.Exec(t.Context(), "UPDATE commands SET status=$2 WHERE id=$1", original.ID, hostclient.CommandFailed); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_runs SET input=$2,start_retry_at=now()-interval '1 minute',start_retry_queued=true WHERE id=$1", first, refusalTestToken()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Owner.Exec(t.Context(), "DELETE FROM host_minutes"); err != nil {
				t.Fatal(err)
			}
			if err := workflow.NewEngine(db.Owner, nil).StartQueued(t.Context()); err != nil {
				t.Fatal(err)
			}
			if runField(t, h, first, "status") != "failed" || runField(t, h, second, "status") != "starting" || runField(t, h, first, "start_retry_queued") != false || runField(t, h, first, "start_retry_at") != nil {
				t.Fatal("refused start retry held up queue")
			}
			var count int
			if err := db.Owner.QueryRow(t.Context(), "SELECT count(*) FROM commands WHERE args->'labels'->>'intelligence.run'=$1 OR args->'agent'->'labels'->>'intelligence.run'=$1", first).Scan(&count); err != nil || count != 1 {
				t.Fatal("refusal persisted an extra startup command")
			}
		})
	}
}

func TestCredentialFinderHandoffIsARefusal(t *testing.T) {
	h, db := dbServer(t)
	b := sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	wf := createWorkflow(t, h, workflow.Starting)
	run := startRun(t, h, wf, "Find project")
	succeed(t, b, b.command(), `{"agentId":"finder-1"}`)
	if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_versions SET definition=jsonb_set(definition,'{orchestrator,instructions}',to_jsonb($2::text)) WHERE workflow_id=$1", wf, refusalTestToken()); err != nil {
		t.Fatal(err)
	}
	code, msg := move(t, h, key, run, map[string]string{"move": "project", "directory": "/w/app"})
	if code != http.StatusBadRequest || strings.Contains(msg, refusalTestToken()) {
		t.Fatal("finder handoff returned an unsafe or retryable error")
	}
	if runField(t, h, run, "status") != "finding_project" {
		t.Fatal("refused handoff changed run state")
	}
}

func TestCredentialVoiceMemoIsIgnoredAndLaterMemoStarts(t *testing.T) {
	db := dbtest.New(t)
	h := newServer(t, db, db.Owner, "v")
	sendReport(t, h, wfReport())
	def := workflow.Starting
	def.Trigger = workflow.TriggerVoiceMemo
	def.VoiceMemo = &workflow.VoiceMemoFilter{}
	createWorkflow(t, h, def)
	memos := []workflow.VoiceMemo{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := "MA"
		if len(memos) > 0 {
			cursor = "Mg"
		}
		json.NewEncoder(w).Encode(map[string]any{"feed": "feed:voice-memos", "next_cursor": cursor, "recordings": memos, "has_more": false})
	}))
	defer srv.Close()
	s := voicememo.Service{DB: db.Owner, Engine: workflow.NewEngine(db.Owner, nil), URL: srv.URL, Key: "test", Username: h.acct.Username}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	memos = []workflow.VoiceMemo{{Source: "apple_voice_memos", Account: "owner", ID: "bad", Title: "Ideas", Transcript: refusalTestToken()}, {Source: "apple_voice_memos", Account: "owner", ID: "good", Title: "Ideas", Transcript: "Prepare deck"}}
	if err := s.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, receipts := sqlPost(t, h, q("SELECT recording_id,decision,reason FROM voice_memos ORDER BY recording_id"))
	if len(receipts.Rows) != 2 || receipts.Rows[0][1] != "ignored" || receipts.Rows[1][1] != "started" {
		t.Fatal("credential memo blocked feed")
	}
	if !strings.Contains(receipts.Rows[0][2].(string), "credential") || strings.Contains(receipts.Rows[0][2].(string), refusalTestToken()) {
		t.Fatal("unsafe refusal reason")
	}
	_, state := sqlPost(t, h, q("SELECT cursor,error FROM voice_memo_polling"))
	if state.Rows[0][0] != "Mg" || state.Rows[0][1] != "" {
		t.Fatal("memo cursor did not advance cleanly")
	}
}
