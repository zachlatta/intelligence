package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// hostReport is wfReport as host, with one repository at root.
func hostReport(host, root string) hostclient.Report {
	rep := wfReport()
	rep.Name = host
	rep.Paseo.Hostname = host
	rep.Paseo.Workspaces[0].ProjectRoot = root
	return rep
}

// startRunOn posts a run of wf with a chosen host and returns the response.
func startRunOn(t *testing.T, h http.Handler, wf, input, host string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"workflowId": wf, "input": input, "host": host})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs", string(body), sameOriginFetch)
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code == http.StatusCreated {
		return rec.Code, out.ID
	}
	return rec.Code, rec.Body.String()
}

func runHost(t *testing.T, h http.Handler, run string) (string, bool) {
	t.Helper()
	code, r := sqlPost(t, h, q(`SELECT host, host_chosen FROM workflow_runs WHERE id = '`+run+`'`))
	if code != http.StatusOK || len(r.Rows) != 1 {
		t.Fatalf("run %s: %d %+v", run, code, r)
	}
	return r.Rows[0][0].(string), r.Rows[0][1].(bool)
}

func TestATaskRunsOnTheHostChosenForIt(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	sendReport(t, h, hostReport("host-2", "/w/app"))
	one, two := openStream(t, h, "host-1"), openStream(t, h, "host-2")
	wf := createWorkflow(t, h, workflow.Starting) // on host-1

	// nothing chosen: the workflow's host, as before
	run := startRun(t, h, wf, "Fix the login page")
	if host, chosen := runHost(t, h, run); host != "host-1" || chosen {
		t.Fatalf("default run on %s (chosen %v), want host-1", host, chosen)
	}
	if c := one.command(); c.Kind != hostclient.CreateAgentKind {
		t.Fatalf("host-1 got %s", c.Kind)
	}

	// host-2 chosen: the run, its finder and everything after are on host-2
	code, run2 := startRunOn(t, h, wf, "Fix the signup page", "host-2")
	if code != http.StatusCreated {
		t.Fatalf("POST with host-2 = %d %s", code, run2)
	}
	if host, chosen := runHost(t, h, run2); host != "host-2" || !chosen {
		t.Fatalf("chosen run on %s (chosen %v), want host-2", host, chosen)
	}
	finder := two.command()
	if finder.Kind != hostclient.CreateAgentKind {
		t.Fatalf("host-2 got %s", finder.Kind)
	}
	succeed(t, two, finder, `{"agentId":"finder-2"}`)
	if code, msg := move(t, h, keyOf(t, h, "host-2"), run2, map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
		t.Fatalf("choose project on host-2: %d %s", code, msg)
	}
	two.command() // archive finder
	if ws := two.command(); ws.Kind != hostclient.CreateWorkspaceKind {
		t.Fatalf("host-2 got %s, want its workspace", ws.Kind)
	}

	// the workflow's own host, named: the same as nothing chosen
	if code, run3 := startRunOn(t, h, wf, "Fix the footer", "host-1"); code != http.StatusCreated {
		t.Fatalf("POST with host-1 = %d %s", code, run3)
	} else if host, chosen := runHost(t, h, run3); host != "host-1" || chosen {
		t.Fatalf("run on its workflow's host = %s (chosen %v)", host, chosen)
	}
}

func TestAChosenHostMustBeTheAccountsAndAbleToRunIt(t *testing.T) {
	h, _ := dbServer(t)
	ts := h.(*testServer)
	sendReport(t, h, wfReport())
	sendReport(t, h, hostReport("host-3", "/w/other"))
	wf := createWorkflow(t, h, workflow.Starting)

	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	joinHost(t, ts.store, bob, "bobs-host")
	for _, host := range []string{"bobs-host", "nowhere", "not a host"} {
		if code, body := startRunOn(t, h, wf, "x", host); code != http.StatusBadRequest {
			t.Errorf("run on %q = %d %s, want 400", host, code, body)
		}
	}
	if code, r := sqlPost(t, h, q(`SELECT count(*) FROM workflow_runs`)); code != http.StatusOK || r.Rows[0][0] != float64(0) {
		t.Fatalf("refused runs were made: %+v", r)
	}

	// a fixed project needs its directory on the chosen host
	d := workflow.Starting
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	fixed := createWorkflow(t, h, d)
	if code, body := startRunOn(t, h, fixed, "x", "host-3"); code != http.StatusBadRequest || !strings.Contains(body, "/w/app") {
		t.Errorf("fixed /w/app on host-3 = %d %s, want 400 naming the directory", code, body)
	}
	sendReport(t, h, hostReport("host-3", "/w/app"))
	three := openStream(t, h, "host-3")
	code, run := startRunOn(t, h, fixed, "x", "host-3")
	if code != http.StatusCreated {
		t.Fatalf("fixed /w/app on host-3 once reported = %d %s", code, run)
	}
	var wa hostclient.CreateWorkspaceArgs
	ws := three.command()
	json.Unmarshal(ws.Args, &wa)
	if ws.Kind != hostclient.CreateWorkspaceKind || wa.Cwd != "/w/app" {
		t.Fatalf("host-3 got %s %+v", ws.Kind, wa)
	}
}

func TestRetryingARunKeepsTheHostChosenForIt(t *testing.T) {
	h, db := dbServer(t)
	sendReport(t, h, wfReport())
	sendReport(t, h, hostReport("host-2", "/w/app"))
	two := openStream(t, h, "host-2")
	wf := createWorkflow(t, h, workflow.Starting)
	code, run := startRunOn(t, h, wf, "anything", "host-2")
	if code != http.StatusCreated {
		t.Fatalf("POST = %d %s", code, run)
	}
	c := two.command()
	if _, err := db.Owner.Exec(t.Context(), "UPDATE workflow_runs SET start_retries = $2 WHERE id = $1", run, workflow.StartRetryLimit); err != nil {
		t.Fatal(err)
	}
	failCommand(t, two, c, "provider claude is not available")
	if r := readRun(t, h, run); r["status"] != "failed" {
		t.Fatalf("run = %v", r)
	}
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/retry", `{}`, sameOriginFetch)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if host, chosen := runHost(t, h, out.ID); host != "host-2" || !chosen {
		t.Fatalf("retry on %s (chosen %v), want host-2", host, chosen)
	}
}

func TestTheTaskComposerOffersTheHostsThatCanRunIt(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	sendReport(t, h, hostReport("host-2", "/w/app"))
	createWorkflow(t, h, workflow.Starting)
	page := get(t, h, "/workflows", nil).Body.String()
	for _, want := range []string{`<select id="task-host"`, `<option value="">Workflow’s host</option>`,
		`<option value="host-1">host-1</option>`, `<option value="host-2">host-2</option>`, `"host": taskHost.value`} {
		if !strings.Contains(page, want) {
			t.Errorf("/workflows lacks %s", want)
		}
	}
}
