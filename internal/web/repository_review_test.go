package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestQueueCreationUpgradeDoesNotRelabelLegacyFinder(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	legacy := startRun(t, h, createWorkflow(t, h, workflow.Starting), "legacy finder")
	b.command()
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute), 97, 0)
	fresh := startRun(t, h, fixedWorkflow(t, h), "not launched yet")
	// Before launch tracking existed, a run could have a finder command but
	// launched_at=NULL. Simulate that run later requeuing after selection.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='queued',launched_at=NULL WHERE id=$1`, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events (run_id,account_id,at,kind,text)
		SELECT id,account_id,now(),'queued','capacity' FROM workflow_runs WHERE id=$1`, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(t.Context(), `DROP VIEW workflow_repository_memory, workflow_run_memory;
		ALTER TABLE workflow_runs DROP COLUMN queued_at_creation, DROP COLUMN project_selected, DROP COLUMN estimated_memory_bytes;
		DELETE FROM schema_migrations WHERE version='0040_workflow_repository_memory'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d.Owner); err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, legacy, "queued_at_creation"); got != false {
		t.Fatalf("legacy finder relabeled Submitted: %v", got)
	}
	if got := runField(t, h, fresh, "queued_at_creation"); got != true {
		t.Fatalf("unlaunched submission not identified: %v", got)
	}
}

func TestRepositoryHistoryIgnoresIncompleteAndUnselectedRuns(t *testing.T) {
	for _, tc := range []struct {
		status, repo string
		selected     bool
	}{
		{"canceled", "/w/app", true}, {"failed", "/w/app", true},
		{"succeeded", "/w/app", false},
		{"canceled", "", false}, {"succeeded", "", true},
	} {
		t.Run(tc.status+tc.repo, func(t *testing.T) {
			h, d := dbServer(t)
			sendReport(t, h, wfReport())
			wf := repositoryHistory(t, h, d, "/w/app")
			if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status=$1,repo=$2,project_selected=$3`, tc.status, tc.repo, tc.selected); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=134217728`); err != nil {
				t.Fatal(err)
			}
			id := startRun(t, h, wf, "new build")
			if got := runField(t, h, id, "estimated_memory_bytes"); got != float64(4<<30) {
				t.Fatalf("incomplete history lowered fallback: %v", got)
			}
			code, result := sqlPost(t, h, q(`SELECT count(*) FROM workflow_repository_memory`))
			if code != http.StatusOK || result.Rows[0][0] != float64(0) {
				t.Fatalf("invalid learning evidence: %+v", result)
			}
		})
	}
}

func TestProjectSelectionCannotUseSoloAdmissionAheadOfOlderQueue(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	b := sendReport(t, h, wfReport())
	repositoryHistory(t, h, d, "/w/app")
	b.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1`, int64(7<<30)); err != nil {
		t.Fatal(err)
	}
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute), 97, 0)
	older := startRun(t, h, fixedWorkflow(t, h), "older queued build")
	newer := startRun(t, h, createWorkflow(t, h, workflow.Starting), "newer matched build")
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+newer+"/start", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	succeed(t, b, b.command(), `{"agentId":"finder"}`)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE host_minutes SET cpu_avg_pct=10,mem_total=$1,mem_available_min=$1`, int64(16<<30)); err != nil {
		t.Fatal(err)
	}
	if code, msg := move(t, h, keyOf(t, h, "host-1"), newer, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("move: %d %s", code, msg)
	}
	if got := runField(t, h, newer, "status"); got != "queued" {
		t.Fatalf("newer run skipped FIFO head %s: %v", older, got)
	}
}

func TestProjectSelectionCannotSkipOlderReadyRetry(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC()}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour; c.Now = clock.Now })
	b := sendReport(t, h, wfReport())
	wf := repositoryHistory(t, h, d, "/w/app")
	b.command()
	if _, err := d.Owner.Exec(t.Context(), `UPDATE agent_minutes SET mem_max=$1`, int64(7<<30)); err != nil {
		t.Fatal(err)
	}
	older := startRun(t, h, wf, "older retry")
	cmd := b.command()
	hostMinute(t, b, clock.Now().Truncate(time.Minute), 97, 0)
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandFailed, Error: "temporary failure"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	clock.Add(16 * time.Second)
	if err := workflow.NewEngine(d.Owner, clock.Now).RetryStarts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := runField(t, h, older, "start_retry_queued"); got != true {
		t.Fatalf("retry not ready: %v", got)
	}
	newer := startRun(t, h, createWorkflow(t, h, workflow.Starting), "newer project")
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+newer+"/start", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	succeed(t, b, b.command(), `{"agentId":"finder"}`)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE host_minutes SET cpu_avg_pct=10,mem_total=$1,mem_available_min=$1`, int64(16<<30)); err != nil {
		t.Fatal(err)
	}
	if code, msg := move(t, h, keyOf(t, h, "host-1"), newer, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("move %d %s", code, msg)
	}
	if got := runField(t, h, newer, "status"); got != "queued" {
		t.Fatalf("jumped older ready retry: %v", got)
	}
}

func TestFixedRepositoryPathsUseCanonicalHistory(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	repositoryHistory(t, h, d, "/w/app")
	b.command()
	repositoryMinute(t, b, 7<<30)
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app/"}
	wf := createWorkflow(t, h, def)
	if _, result := sqlPost(t, h, q(`SELECT definition->'project'->>'directory' FROM workflow_versions WHERE workflow_id='`+wf+`'`)); result.Rows[0][0] != "/w/app" {
		t.Fatalf("saved definition is not canonical: %+v", result)
	}
	// Also exercise a definition saved before path normalization was added.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_versions SET definition=jsonb_set(definition,'{project,directory}','"/w/app/"') WHERE workflow_id=$1`, wf); err != nil {
		t.Fatal(err)
	}
	id := startRun(t, h, wf, "canonical repository")
	if got := runField(t, h, id, "estimated_memory_bytes"); got != float64(10<<30) {
		t.Fatalf("path missed history: %v", got)
	}
	if got := runField(t, h, id, "repo"); got != "/w/app" {
		t.Fatalf("unclean path: %v", got)
	}
}

func TestNewLiveWorkerIsCapturedOnItsFirstReport(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	wf := repositoryHistory(t, h, d, "/w/app")
	b.command()
	id := startRun(t, h, wf, "active build")
	succeed(t, b, b.command(), `{"workspaceId":"w-app","agentId":"`+agentA1+`"}`)
	rep := wfReport()
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	rep.Paseo.Agents = append(append([]hostclient.Agent{}, rep.Paseo.Agents...), hostclient.Agent{ID: "new-worker", WorkspaceID: "w-docs", ParentID: agentA1, Status: "running"})
	if ack := b.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	code, result := sqlPost(t, h, q(`SELECT count(*) FROM workflow_run_agents WHERE run_id='`+id+`' AND agent_id='new-worker'`))
	if code != http.StatusOK || result.Rows[0][0] != float64(1) {
		t.Fatalf("worker missing on first report: %+v", result)
	}
	minute := time.Now().UTC().Truncate(time.Minute)
	cpu := 10.0
	frame := resources.Report{Minutes: []resources.Minute{{Minute: minute, Samples: 12, CPUs: 8, CPUAvgPct: &cpu, Load1: 1, MemTotal: 16 << 30, MemAvailableMin: 8 << 30}}, Agents: []resources.AgentMinute{{Minute: minute, AgentID: "new-worker", MemMax: 8 << 30}}}
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &frame}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if _, result := sqlPost(t, h, q(`SELECT peak_memory_bytes FROM workflow_run_memory WHERE run_id='`+id+`'`)); len(result.Rows) != 1 || result.Rows[0][0] != float64(8<<30) {
		t.Fatalf("live worker absent from SQL evidence: %+v", result)
	}
	def := workflow.Starting
	def.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/desk"}
	next := startRun(t, h, createWorkflow(t, h, def), "small work")
	if got := runField(t, h, next, "status"); got != "starting" {
		t.Fatalf("live worker RSS counted twice: %v", got)
	}
}

func TestMatchedProjectSelectionIsVisibleWhileQueued(t *testing.T) {
	h, d := dbServer(t)
	b := sendReport(t, h, wfReport())
	repositoryHistory(t, h, d, "/w/app")
	b.command()
	id := startRun(t, h, createWorkflow(t, h, workflow.Starting), "find project")
	succeed(t, b, b.command(), `{"agentId":"finder"}`)
	repositoryMinute(t, b, 7<<30)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), id, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("move: %d %s", code, msg)
	}
	page := get(t, h, "/workflow-runs/"+id, nil).Body.String()
	for _, bad := range []string{` @ <span class="mono"></span>`, `, worktree <span class="mono"></span>`, "Starts once the project is found.", "<p>Submitted</p>"} {
		if strings.Contains(page, bad) {
			t.Errorf("misleading queued UI: %s", bad)
		}
	}
	if !strings.Contains(page, "Orchestrator is waiting for host capacity.") {
		t.Error("orchestrator wait hidden")
	}
	code, result := sqlPost(t, h, q(`SELECT count(*) FROM workflow_run_events WHERE run_id='`+id+`' AND kind='project'`))
	if code != http.StatusOK || result.Rows[0][0] != float64(1) {
		t.Fatalf("project selection not recorded: %+v", result)
	}
	repositoryMinute(t, b, 16<<30)
	_, result = sqlPost(t, h, q(`SELECT count(*) FROM workflow_run_events WHERE run_id='`+id+`' AND kind='project'`))
	if result.Rows[0][0] != float64(1) {
		t.Fatal("project event duplicated on admission")
	}
}
