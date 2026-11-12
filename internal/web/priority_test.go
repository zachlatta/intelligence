package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWorkflowPriorityAPIAndQueue(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 97, .5)
	wf := fixedWorkflow(t, h)
	first := startRun(t, h, wf, "normal first")
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	body := strings.Replace(wfBody(t, "Urgent work", def), `"definition":{`, `"definition":{"priority":"immediate",`, 1)
	rec := call(t, h, http.MethodPut, "/api/workflows/"+wf, body, sameOriginFetch)
	if rec.Code != http.StatusOK {
		t.Fatalf("save priority: %d %s", rec.Code, rec.Body)
	}
	urgent := startRun(t, h, wf, "urgent")
	if got := runField(t, h, urgent, "status"); got != "starting" {
		t.Fatalf("immediate run = %v", got)
	}
	if got := runField(t, h, first, "status"); got != "queued" {
		t.Fatalf("normal run = %v", got)
	}
	code, rows := sqlPost(t, h, q("SELECT v.definition->>'priority' FROM workflow_runs r JOIN workflow_versions v USING(workflow_id,version) WHERE r.id='"+urgent+"'"))
	if code != 200 || rows.Rows[0][0] != "immediate" {
		t.Fatalf("SQL priority: %d %+v", code, rows)
	}
	// Saving normal restores admission checks, preserving the earlier run's place.
	body = strings.Replace(body, `"priority":"immediate"`, `"priority":"normal"`, 1)
	if rec = call(t, h, http.MethodPut, "/api/workflows/"+wf, body, sameOriginFetch); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	second := startRun(t, h, wf, "normal second")
	code, rows = sqlPost(t, h, q("SELECT run_id::text FROM workflow_run_queue ORDER BY position"))
	if code != 200 || len(rows.Rows) != 2 || rows.Rows[0][0] != first || rows.Rows[1][0] != second {
		t.Fatalf("normal FIFO: %+v", rows)
	}
	body = strings.Replace(body, `"priority":"normal"`, `"priority":"invalid"`, 1)
	if rec = call(t, h, http.MethodPut, "/api/workflows/"+wf, body, sameOriginFetch); rec.Code != 400 {
		t.Fatalf("invalid priority: %d", rec.Code)
	}
	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestPriorityChangeAppearsInProposalReview(t *testing.T) {
	a := &workflow.WorkflowState{Definition: workflow.Definition{Priority: workflow.PriorityNormal}}
	b := &workflow.WorkflowState{Definition: workflow.Definition{Priority: workflow.PriorityImmediate}}
	changes := proposalChanges(a, b)
	if len(changes) != 1 || changes[0].Label != "Priority" || changes[0].After != "immediate" {
		t.Fatalf("priority review: %+v", changes)
	}
}

func TestImmediateRetryPassesOlderNormalRun(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC()}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	hostMinute(t, b, clock.Now().Truncate(time.Minute), 97, .5)
	normal := startRun(t, h, fixedWorkflow(t, h), "older normal")
	def := workflow.Starting
	def.Priority = workflow.PriorityImmediate
	wf := createWorkflow(t, h, def)
	run := startRun(t, h, wf, "urgent retry")
	command := b.command()
	ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{
		ID: command.ID, Status: hostclient.CommandFailed, Error: "temporary error",
	}})
	if ack.Error != "" {
		t.Fatal(ack.Error)
	}
	clock.Add(16 * time.Second)
	e := workflow.NewEngine(d.Owner, clock.Now)
	if err := e.RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, run, "start_retries"); got != float64(1) {
		t.Fatalf("urgent retry blocked: %v", got)
	}
	if got := runField(t, h, normal, "status"); got != "queued" {
		t.Fatalf("normal = %v", got)
	}
}

func TestEmailAndVoiceMemoPriorityDefaultsStartImmediately(t *testing.T) {
	for _, trigger := range []string{workflow.TriggerEmail, workflow.TriggerVoiceMemo} {
		t.Run(trigger, func(t *testing.T) {
			d := dbtest.New(t)
			h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
			sendReport(t, h, wfReport())
			b := openStream(t, h, "host-1")
			hostMinute(t, b, time.Now().UTC().Truncate(time.Minute), 97, .5)
			def := workflow.Starting
			wf := createWorkflow(t, h, def)
			// The editor opens the saved task definition before switching its trigger.
			var saved []byte
			if err := d.Owner.QueryRow(t.Context(), `SELECT v.definition FROM workflows w JOIN workflow_versions v ON v.workflow_id=w.id AND v.version=w.version WHERE w.id=$1`, wf).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(saved, &def); err != nil {
				t.Fatal(err)
			}
			def.Trigger = trigger
			if trigger == workflow.TriggerEmail {
				def.Email = &workflow.EmailFilter{From: []string{"sender@example.com"}}
			} else {
				def.VoiceMemo = &workflow.VoiceMemoFilter{}
			}
			if rec := call(t, h, http.MethodPut, "/api/workflows/"+wf, wfBody(t, "Automatic requests", def), sameOriginFetch); rec.Code != 200 {
				t.Fatal(rec.Body)
			}
			run := startRun(t, h, wf, "new request")
			if got := runField(t, h, run, "status"); got == "queued" {
				t.Fatal("default request queued")
			}
			code, rows := sqlPost(t, h, q("SELECT coalesce(v.definition->>'priority',''),r.priority FROM workflow_runs r JOIN workflow_versions v USING(workflow_id,version) WHERE r.id='"+run+"'"))
			if code != 200 || len(rows.Rows) != 1 || rows.Rows[0][0] != "" || rows.Rows[0][1] != "immediate" {
				t.Fatalf("configured/effective priorities: %+v", rows)
			}
			def.Priority = workflow.PriorityNormal
			if rec := call(t, h, http.MethodPut, "/api/workflows/"+wf, wfBody(t, "Normal requests", def), sameOriginFetch); rec.Code != 200 {
				t.Fatal(rec.Body)
			}
			run = startRun(t, h, wf, "normal request")
			if got := runField(t, h, run, "status"); got != "queued" {
				t.Fatalf("normal request = %v", got)
			}
			code, rows = sqlPost(t, h, q("SELECT priority FROM workflow_runs WHERE workflow_id='"+wf+"' ORDER BY created_at"))
			if code != 200 || len(rows.Rows) != 2 || rows.Rows[0][0] != "immediate" || rows.Rows[1][0] != "normal" {
				t.Fatalf("run priorities changed after edit: %+v", rows)
			}
		})
	}
}
