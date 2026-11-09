package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// wfReport is host-1 with one repository, /w/app (its workspaces' project root).
func wfReport() hostclient.Report {
	rep := sampleReport
	rep.Paseo.Workspaces = append([]hostclient.Workspace(nil), sampleReport.Paseo.Workspaces...)
	rep.Paseo.Workspaces[0].ProjectRoot = "/w/app"
	return rep
}

// wfBody is a workflow request for d, on host-1 unless d names a host.
func wfBody(t *testing.T, name string, d workflow.Definition) string {
	t.Helper()
	if d.Host == "" {
		d.Host = "host-1"
	}
	b, err := json.Marshal(map[string]any{"name": name, "description": "for a test", "definition": d})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// createWorkflow makes the starting workflow (or d) and returns its id.
func createWorkflow(t *testing.T, h http.Handler, d workflow.Definition) string {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "Task from Intelligence", d), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/workflows = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.ID == "" || out.Version != 1 {
		t.Fatalf("created = %s", rec.Body)
	}
	return out.ID
}

func startRun(t *testing.T, h http.Handler, wf, input string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"workflowId": wf, "input": input})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs", string(body), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/workflow-runs = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Header().Get("Location") != "/workflow-runs/"+out.ID {
		t.Errorf("location = %q", rec.Header().Get("Location"))
	}
	return out.ID
}

// move is what `intelligence workflow` sends, with host's key.
func move(t *testing.T, h http.Handler, key, run string, m map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(m)
	rec := call(t, anon(h), http.MethodPost, "/api/workflow-runs/"+run+"/moves", string(body), map[string]string{"Authorization": "Bearer " + key})
	var out struct {
		Message string `json:"message"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Message == "" {
		out.Message = rec.Body.String()
	}
	return rec.Code, out.Message
}

// readRun reads the run through the account's SQL API.
func readRun(t *testing.T, h http.Handler, run string) map[string]any {
	t.Helper()
	code, r := sqlPost(t, h, q(`SELECT status, step, repo, branch, workspace_id, orchestrator_id, project_agent_id, note, error
		FROM workflow_runs WHERE id = '`+run+`'`))
	if code != http.StatusOK || len(r.Rows) != 1 {
		t.Fatalf("run %s: %d %+v", run, code, r)
	}
	out := map[string]any{}
	for i, c := range r.Columns {
		out[c.Name] = r.Rows[0][i]
	}
	return out
}

// succeed reports cmd succeeded with result, as the host would.
func succeed(t *testing.T, b *rawHost, cmd hostclient.Command, result string) {
	t.Helper()
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: cmd.ID, Status: hostclient.CommandSucceeded,
		Result: json.RawMessage(result)}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

// commandsByKind reads n commands off b's stream.
func commandsByKind(t *testing.T, b *rawHost, n int) map[string]hostclient.Command {
	t.Helper()
	out := map[string]hostclient.Command{}
	for range n {
		c := b.command()
		out[c.Kind] = c
	}
	return out
}

func TestAWorkflowRunFindsItsProjectThenTheOrchestratorMovesItThroughTheSteps(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	wf := createWorkflow(t, h, workflow.Starting)

	// editing makes a new version; a run uses the latest
	edited := workflow.Starting
	edited.Steps = append([]workflow.Step(nil), workflow.Starting.Steps...)
	edited.Steps[0].Instructions = "Have sub-agents do this in {{repo}}:\n\n{{input}}"
	rec := call(t, h, http.MethodPut, "/api/workflows/"+wf, wfBody(t, "Tasks", edited), sameOriginFetch)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":2`) {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}

	run := startRun(t, h, wf, "Show each agent's model in the tree")
	finder := b.command()
	var fa hostclient.CreateAgentArgs
	json.Unmarshal(finder.Args, &fa)
	if finder.Kind != hostclient.CreateAgentKind || fa.Cwd != "~" || fa.Provider != "claude" || fa.Labels["intelligence.run"] != run ||
		!strings.Contains(fa.Prompt, "- /w/app") || !strings.Contains(fa.Prompt, "intelligence workflow project --run "+run) {
		t.Fatalf("project finder = %s %+v", finder.Kind, fa)
	}
	if r := readRun(t, h, run); r["status"] != "finding_project" {
		t.Fatalf("run = %v", r)
	}
	succeed(t, b, finder, `{"agentId":"finder-1"}`)

	if code, msg := move(t, h, key, run, map[string]string{"move": "project", "directory": "/somewhere/else"}); code != http.StatusBadRequest {
		t.Errorf("a directory that is not one of the host's repositories: %d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "next"}); code != http.StatusConflict {
		t.Errorf("next before the project is found: %d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("project: %d %s", code, msg)
	}
	cmds := commandsByKind(t, b, 2)
	if a := cmds[hostclient.ArchiveAgentKind]; !strings.Contains(string(a.Args), "finder-1") {
		t.Errorf("the finder should be archived: %+v", cmds)
	}
	ws := cmds[hostclient.CreateWorkspaceKind]
	var wa hostclient.CreateWorkspaceArgs
	json.Unmarshal(ws.Args, &wa)
	if wa.Cwd != "/w/app" || !wa.Worktree || !strings.HasPrefix(wa.Branch, "wf-") || wa.Agent == nil || wa.Agent.Thinking != "high" ||
		wa.Agent.Labels["intelligence.run"] != run || !strings.Contains(wa.Agent.Prompt, "Step 1 of 6: Do the work") ||
		!strings.Contains(wa.Agent.Prompt, "Have sub-agents do this in /w/app:") || !strings.Contains(wa.Agent.Prompt, "intelligence workflow next --run "+run) {
		t.Fatalf("workspace = %+v %+v", wa, wa.Agent)
	}
	if r := readRun(t, h, run); r["status"] != "starting" || r["repo"] != "/w/app" || r["project_agent_id"] != "finder-1" {
		t.Fatalf("run = %v", r)
	}
	succeed(t, b, ws, `{"workspaceId":"w-run","agentId":"orch-1"}`)
	if r := readRun(t, h, run); r["status"] != "running" || r["step"] != float64(0) || r["orchestrator_id"] != "orch-1" || r["workspace_id"] != "w-run" {
		t.Fatalf("run = %v", r)
	}

	// another host's key moves nothing
	if code, _ := move(t, h, keyOf(t, h, "host-2"), run, map[string]string{"move": "next"}); code != http.StatusNotFound {
		t.Errorf("another host's key: %d", code)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "next", "summary": "built it"}); code != http.StatusOK ||
		!strings.Contains(msg, "Step 2 of 6: Meets the request") || !strings.Contains(msg, "fresh sub-agent") {
		t.Fatalf("next: %d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "back", "step": "meets", "why": "x"}); code != http.StatusBadRequest {
		t.Errorf("back to the current step: %d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "back", "step": "build", "why": "the transcript page shows no model"}); code != http.StatusOK ||
		!strings.Contains(msg, "Step 1 of 6: Do the work") || !strings.Contains(msg, "the transcript page shows no model") {
		t.Fatalf("back: %d %s", code, msg)
	}
	for range 4 {
		if code, msg := move(t, h, key, run, map[string]string{"move": "next", "summary": "ok"}); code != http.StatusOK {
			t.Fatalf("next: %d %s", code, msg)
		}
	}
	if r := readRun(t, h, run); r["step"] != float64(4) {
		t.Fatalf("at step %v, want the sign-off (4)", r["step"])
	}
	note := "## Adds a view\n\nBefore and after.\n\n<script>alert(1)</script>"
	if code, msg := move(t, h, key, run, map[string]string{"move": "ask", "note": note}); code != http.StatusOK || !strings.Contains(msg, "End your turn") {
		t.Fatalf("ask: %d %s", code, msg)
	}
	if r := readRun(t, h, run); r["status"] != "waiting" || r["note"] != note {
		t.Fatalf("run = %v", r)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "next"}); code != http.StatusConflict || !strings.Contains(msg, "Zach has not replied") {
		t.Errorf("next while waiting: %d %s", code, msg)
	}
	page := get(t, h, "/workflow-runs/"+run, nil)
	if page.Code != http.StatusOK {
		t.Fatalf("run page = %d", page.Code)
	}
	for _, want := range []string{"Show each agent&#39;s model in the tree", "<h2>Adds a view</h2>", "&lt;script&gt;", "Meets the request",
		"Zach&#39;s sign-off", "/w/app", "the transcript page shows no model", `id="reply"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("run page misses %q", want)
		}
	}
	if strings.Contains(page.Body.String(), "<script>alert") {
		t.Error("the note's raw HTML should show as text")
	}

	rec = call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/reply", `{"text":"Ship it"}`, sameOriginFetch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reply = %d %s", rec.Code, rec.Body)
	}
	msg := b.command()
	var ma hostclient.AgentArgs
	json.Unmarshal(msg.Args, &ma)
	if msg.Kind != hostclient.SendMessageKind || ma.AgentID != "orch-1" || !strings.Contains(ma.Text, "Ship it") {
		t.Fatalf("reply message = %s %+v", msg.Kind, ma)
	}
	if r := readRun(t, h, run); r["status"] != "running" || r["note"] != "" {
		t.Fatalf("run = %v", r)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "next", "summary": "approved"}); code != http.StatusOK || !strings.Contains(msg, "Step 6 of 6: Land and deploy") {
		t.Fatalf("next: %d %s", code, msg)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "next", "summary": "landed"}); code != http.StatusOK || !strings.Contains(msg, "finished") {
		t.Fatalf("last next: %d %s", code, msg)
	}
	if r := readRun(t, h, run); r["status"] != "succeeded" {
		t.Fatalf("run = %v", r)
	}
	if code, _ := move(t, h, key, run, map[string]string{"move": "next"}); code != http.StatusConflict {
		t.Errorf("a finished run moved: %d", code)
	}

	code, r := sqlPost(t, h, q(`SELECT string_agg(kind, ' ' ORDER BY id) FROM workflow_run_events WHERE run_id = '`+run+`'`))
	want := "started project workspace next back next next next next ask reply next done"
	if code != http.StatusOK || len(r.Rows) != 1 || r.Rows[0][0] != want {
		t.Errorf("events = %v, want %q", r.Rows, want)
	}
}

func TestAFixedProjectStartsTheOrchestratorAtOnce(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	d := workflow.Starting
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	run := startRun(t, h, createWorkflow(t, h, d), "Fix the login page")
	ws := b.command()
	if ws.Kind != hostclient.CreateWorkspaceKind || !strings.Contains(string(ws.Args), `"cwd":"/w/app"`) {
		t.Fatalf("command = %s %s", ws.Kind, ws.Args)
	}
	if r := readRun(t, h, run); r["status"] != "starting" || r["repo"] != "/w/app" {
		t.Fatalf("run = %v", r)
	}
}

func TestNoProjectRunsTheOrchestratorInTheHomeDirectory(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "What did we pay for hosting in October?")
	b.command()
	if code, msg := move(t, h, key, run, map[string]string{"move": "project", "directory": "none"}); code != http.StatusOK {
		t.Fatalf("project none: %d %s", code, msg)
	}
	ws := b.command()
	var wa hostclient.CreateWorkspaceArgs
	json.Unmarshal(ws.Args, &wa)
	if ws.Kind != hostclient.CreateWorkspaceKind || wa.Cwd != "~" || wa.Worktree || wa.Agent == nil {
		t.Fatalf("command = %s %+v", ws.Kind, wa)
	}
}

func TestAFailedStartFailsTheRun(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "anything")
	c := b.command()
	if ack := b.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: c.ID, Status: hostclient.CommandFailed,
		Error: "provider claude is not available"}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if r := readRun(t, h, run); r["status"] != "failed" || !strings.Contains(r["error"].(string), "provider claude is not available") {
		t.Fatalf("run = %v", r)
	}
}

func TestCancellingARunStopsItsOrchestrator(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	d := workflow.Starting
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	run := startRun(t, h, createWorkflow(t, h, d), "anything")
	succeed(t, b, b.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	if c := b.command(); c.Kind != hostclient.CancelAgentKind || !strings.Contains(string(c.Args), "orch-1") {
		t.Errorf("command = %s %s", c.Kind, c.Args)
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "next"}); code != http.StatusConflict || !strings.Contains(msg, "canceled") {
		t.Errorf("a canceled run moved: %d %s", code, msg)
	}
}

func TestWorkflowsAreTheAccountsOwn(t *testing.T) {
	h, _ := dbServer(t)
	ts := h.(*testServer)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	run := startRun(t, h, wf, "a secret task")
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	asBob := sessionFor(t, ts.store, bob)
	for _, tbl := range []string{"workflows", "workflow_versions", "workflow_runs", "workflow_run_events"} {
		rec := call(t, h, http.MethodGet, "/api/sql?q=SELECT+count(*)+FROM+"+tbl, "", nil, asBob)
		if !strings.Contains(rec.Body.String(), `"rows":[[0]]`) {
			t.Errorf("bob reads someone's %s: %s", tbl, rec.Body)
		}
	}
	body, _ := json.Marshal(map[string]string{"workflowId": wf, "input": "x"})
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs", string(body), sameOriginFetch, asBob); rec.Code != http.StatusNotFound {
		t.Errorf("bob ran someone's workflow: %d", rec.Code)
	}
	if rec := call(t, h, http.MethodPut, "/api/workflows/"+wf, wfBody(t, "mine now", workflow.Starting), sameOriginFetch, asBob); rec.Code != http.StatusNotFound {
		t.Errorf("bob edited someone's workflow: %d", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/reply", `{"text":"x"}`, sameOriginFetch, asBob); rec.Code != http.StatusNotFound {
		t.Errorf("bob replied in someone's run: %d", rec.Code)
	}
	if rec := call(t, h, http.MethodGet, "/workflow-runs/"+run, "", nil, asBob); rec.Code != http.StatusNotFound {
		t.Errorf("bob read someone's run page: %d", rec.Code)
	}
	// a workflow runs only on a host of its own account
	joinHost(t, ts.store, bob, "bobs-host")
	d := workflow.Starting
	d.Host = "bobs-host"
	if rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "on bob's host", d), sameOriginFetch); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "bobs-host") {
		t.Errorf("a workflow on another account's host: %d %s", rec.Code, rec.Body)
	}
}

func TestAWorkflowThatCannotRunIsRefused(t *testing.T) {
	h, _ := dbServer(t)
	d := workflow.Starting
	d.Steps = nil
	if rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "empty", d), sameOriginFetch); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "at least one step") {
		t.Errorf("POST = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, " ", workflow.Starting), sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Errorf("no name: %d", rec.Code)
	}
}

func TestWorkflowPages(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	list := get(t, h, "/workflows", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "starting workflow") {
		t.Fatalf("/workflows with none = %d", list.Code)
	}
	wf := createWorkflow(t, h, workflow.Starting)
	startRun(t, h, wf, "Show each agent's model")
	list = get(t, h, "/workflows", nil)
	for _, want := range []string{"Task from Intelligence", "Show each agent&#39;s model", "finding the project", `href="/workflows/` + wf + `"`, "host-1",
		// the new task box says which workflow it runs, and where
		`<label for="task-workflow">Workflow</label>`, `<option value="` + wf + `">Task from Intelligence · host-1</option>`} {
		if !strings.Contains(list.Body.String(), want) {
			t.Errorf("/workflows misses %q", want)
		}
	}
	ed := get(t, h, "/workflows/"+wf, nil)
	if ed.Code != http.StatusOK || !strings.Contains(ed.Body.String(), "Meets the request") || !strings.Contains(ed.Body.String(), `"version":1`) {
		t.Errorf("editor = %d", ed.Code)
	}
	if !strings.Contains(ed.Body.String(), `"host":"host-1"`) || !strings.Contains(ed.Body.String(), `const hosts = ["host-1"]`) {
		t.Errorf("the editor should show the workflow's host and offer the account's")
	}
	// a fixed project is picked from the repositories the account's hosts reported
	if !strings.Contains(ed.Body.String(), `const allRepos = [{"directory":"/w/app","project":"example/app","hosts":["host-1"]}]`) {
		t.Errorf("the editor should offer the hosts' repositories")
	}
	if rec := get(t, h, "/workflows/00000000-0000-4000-8000-000000000000", nil); rec.Code != http.StatusNotFound {
		t.Errorf("no such workflow = %d", rec.Code)
	}
	if !strings.Contains(get(t, h, "/", nil).Body.String(), `href="/workflows"`) {
		t.Error("the top bar should link to Workflows")
	}
}

// TestAWorkflowSavedWithoutAHostAsksForOne: workflows saved before they named their host do not run
// until it is set in the editor.
func TestAWorkflowSavedWithoutAHostAsksForOne(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_versions SET definition = definition - 'host'"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"workflowId": wf, "input": "x"})
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs", string(body), sameOriginFetch); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "host") {
		t.Errorf("start = %d %s", rec.Code, rec.Body)
	}
}
