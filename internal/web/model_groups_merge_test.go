package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestModelTokensValidateDirectAndReviewedWorkflowWrites(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	def := workflow.Starting
	def.Steps = append([]workflow.Step(nil), def.Steps...)
	def.Steps[0].Instructions = "Use {{frontier_model}}."
	if rec := call(t, h, "POST", "/api/workflows", wfBody(t, "Unknown", def), sameOriginFetch); rec.Code != 400 {
		t.Fatalf("direct unknown token: %d %s", rec.Code, rec.Body)
	}
	propose := func(want int) string {
		t.Helper()
		body := `{"operation":"create","summary":"Use model group","workflow":` + wfBody(t, "Reviewed", def) + `}`
		rec := call(t, anon(h), "POST", "/api/workflow-proposals", body, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
		if rec.Code != want {
			t.Fatalf("proposed token: %d %s", rec.Code, rec.Body)
		}
		var p struct{ ID string }
		json.Unmarshal(rec.Body.Bytes(), &p)
		return p.ID
	}
	propose(400)
	rec := call(t, h, "POST", "/api/model-groups", `{"name":"Frontier","members":[]}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	var group workflow.ModelGroup
	json.Unmarshal(rec.Body.Bytes(), &group)
	id := propose(201)
	// A pending proposal is not a workflow. Approval must revalidate its references.
	if rec := call(t, h, "DELETE", "/api/model-groups/"+group.ID, "", sameOriginFetch); rec.Code != 204 {
		t.Fatal(rec.Body)
	}
	if rec := call(t, h, "POST", "/api/workflow-proposals/"+id+"/approve", `{}`, sameOriginFetch); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unknown model token") {
		t.Fatalf("approval after group deletion: %d %s", rec.Code, rec.Body)
	}
}

func TestModelTokensInApprovedQueuedRunSummaryAndStartRetry(t *testing.T) {
	h, d := dbServer(t)
	rep := wfReport()
	rep.StartRetryKeys = true
	rep.Paseo.Providers = []hostclient.Provider{{Name: "codex", Available: true, Models: []hostclient.Model{{ID: "sol", Label: "GPT Sol", Thinking: []string{"high"}}}}}
	b := sendReport(t, h, rep)
	for _, query := range []string{
		`INSERT INTO usage_accounts(provider,account,label,first_seen,last_seen) VALUES('codex','default','Test plan',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','codex','default',true,now())`,
		`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','codex','default',now(),now())`,
		`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed) VALUES('codex','default','7d',604800,30,now()+interval '1 day',now(),now())`,
	} {
		if _, err := d.Owner.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	rec := call(t, h, "POST", "/api/model-groups", `{"name":"Frontier","members":[{"provider":"codex","model":"sol","thinking":"high","login":"default","longWindow":"7d","enabled":true}]}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	def.Steps = append([]workflow.Step(nil), def.Steps...)
	def.Steps[0].Instructions = "Build with {{frontier_model}}. Task: {{input}}"
	wf := createWorkflow(t, h, def)
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 97, 0.5)
	key := keyOf(t, h, "host-1")
	rec = call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"run","workflowId":"`+wf+`","summary":"Try the group","input":"Keep {{frontier_model}} literal"}`, map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	var proposal struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &proposal)
	rec = call(t, h, "POST", "/api/workflow-proposals/"+proposal.ID+"/approve", `{}`, sameOriginFetch)
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	var run string
	if err := d.Owner.QueryRow(t.Context(), "SELECT started_run_id::text FROM workflow_proposals WHERE id=$1", proposal.ID).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if runField(t, h, run, "status") != "queued" {
		t.Fatal("approved run bypassed queue")
	}
	count := func(want int) {
		t.Helper()
		var got int
		if err := d.Owner.QueryRow(t.Context(), "SELECT count(*) FROM workflow_model_suggestions WHERE run_id=$1", run).Scan(&got); err != nil || got != want {
			t.Fatalf("suggestions: %d want %d: %v", got, want, err)
		}
	}
	count(0)
	if code, msg := move(t, h, key, run, map[string]string{"move": "summarize", "summary": "Build with a balanced model"}); code != 200 {
		t.Fatalf("summary: %d %s", code, msg)
	}
	count(0)
	if runField(t, h, run, "status") != "queued" {
		t.Fatal("summary started queued run")
	}
	if rec := call(t, h, "POST", "/api/workflow-runs/"+run+"/start", `{}`, sameOriginFetch); rec.Code != 204 {
		t.Fatal(rec.Body)
	}
	checkPrompt := func(cmd hostclient.Command) {
		t.Helper()
		var ws hostclient.CreateWorkspaceArgs
		if err := json.Unmarshal(cmd.Args, &ws); err != nil || ws.Agent == nil {
			t.Fatalf("workspace: %s %v", cmd.Args, err)
		}
		if !strings.Contains(ws.Agent.Prompt, "Build with GPT Sol (High) via codex/sol --thinking high on Test plan.") || !strings.Contains(ws.Agent.Prompt, "Keep {{frontier_model}} literal") || !strings.Contains(ws.Agent.Prompt, "workflow summarize") {
			t.Fatalf("unexpanded or incomplete prompt: %s", ws.Agent.Prompt)
		}
	}
	cmd := b.command()
	checkPrompt(cmd)
	count(1)
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: "daemon unavailable"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var due time.Time
	if err := d.Owner.QueryRow(t.Context(), "SELECT start_retry_at FROM workflow_runs WHERE id=$1", run).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), "UPDATE host_minutes SET cpu_avg_pct=0"); err != nil {
		t.Fatal(err)
	}
	if err := workflow.NewEngine(d.Owner, func() time.Time { return due }).RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkPrompt(b.command())
	count(2)
	if page := get(t, h, "/workflow-runs/"+run, nil).Body.String(); !strings.Contains(page, "Build with a balanced model") || !strings.Contains(page, "Suggested GPT Sol (High) on Test plan for {{frontier_model}}") {
		t.Fatal("run page lost summary or suggestion")
	}
}
