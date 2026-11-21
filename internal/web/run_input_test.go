package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestWorkflowRunInputAPI(t *testing.T) {
	h, d := dbServer(t)
	host := sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	run := startRun(t, h, fixedWorkflow(t, h), "Improve the help page")
	path := "/api/workflow-runs/" + run + "/input"
	postInput := func(body string, status int) {
		t.Helper()
		if rec := call(t, h, http.MethodPost, path, body, sameOriginFetch); rec.Code != status {
			t.Fatalf("input = %d %s, want %d", rec.Code, rec.Body, status)
		}
	}
	const first = "Please include a keyboard shortcuts section."
	const body = `{"text":"` + first + `"}`
	postInput(body, http.StatusConflict)
	succeed(t, host, host.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	postInput(`{"text":"  \n  "}`, http.StatusBadRequest)
	postInput(`{"text":"a\u0000b"}`, http.StatusBadRequest)
	postInput(`{"text":"ibk_`+strings.Repeat("x", 43)+`"}`, http.StatusBadRequest)

	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	asBob := sessionFor(t, ts.store, bob)
	if rec := call(t, h, http.MethodPost, path, body, sameOriginFetch, asBob); rec.Code != http.StatusNotFound {
		t.Fatalf("another account's input = %d", rec.Code)
	}
	if rec := call(t, anon(h), http.MethodPost, path, body, map[string]string{"Authorization": "Bearer " + key}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("host key input = %d", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, path, body, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site input = %d", rec.Code)
	}

	postInput(body, http.StatusNoContent)
	cmd := host.command()
	var args hostclient.AgentArgs
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != hostclient.SendMessageKind || args.AgentID != "orch-1" || !strings.Contains(args.Text, first) {
		t.Fatalf("input command = %s %+v", cmd.Kind, args)
	}
	if r := readRun(t, h, run); r["status"] != workflow.Running || r["step"] != float64(0) || r["note"] != "" {
		t.Fatalf("input moved the run: %v", r)
	}
	failCommand(t, host, cmd, "not connected to the Paseo daemon")

	// Sending a note to a stalled run wakes the same orchestrator and carries lost input.
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='stalled', error='idle' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	if strings.Count(page, `id="additional-input"`) != 1 || !strings.Contains(page, "Delivery failed") {
		t.Fatal("stalled page should show one input box and the failed delivery")
	}
	const second = "Include a print layout too."
	postInput(`{"text":"`+second+`"}`, http.StatusNoContent)
	retry := host.command()
	if text := commandText(t, retry); retry.Kind != hostclient.SendMessageKind ||
		!strings.Contains(text, args.Text) || !strings.Contains(text, second) {
		t.Fatalf("send did not carry the previous input verbatim: %s %s", retry.Kind, text)
	}
	if r := readRun(t, h, run); r["status"] != workflow.Running || r["step"] != float64(0) || r["error"] != "" {
		t.Fatalf("run did not resume in place: %v", r)
	}
	succeed(t, host, retry, `{}`)

	if code, msg := move(t, h, key, run, map[string]string{"move": "ask", "note": "Proceed?"}); code != http.StatusOK {
		t.Fatalf("ask = %d %s", code, msg)
	}
	postInput(body, http.StatusConflict)
	if r := readRun(t, h, run); r["status"] != workflow.Waiting || r["note"] != "Proceed?" {
		t.Fatalf("input changed the pending question: %v", r)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	postInput(body, http.StatusConflict)

	code, result := sqlPost(t, h, q(`SELECT e.text, i.command_id::text, i.delivered, i.pending, i.redeliveries
		FROM workflow_run_inputs i JOIN workflow_run_events e ON e.id=i.event_id
		WHERE i.kind='input' AND i.run_id=`+sqlString(run)+` ORDER BY i.event_id`))
	if code != http.StatusOK || len(result.Rows) != 2 {
		t.Fatalf("input history = %d %+v", code, result)
	}
	if row := result.Rows[0]; row[0] != first || row[1] != cmd.ID || row[2] != true || row[3] != false || row[4] != float64(1) {
		t.Fatalf("first input history row = %v", row)
	}
	if rec := call(t, h, http.MethodPost, "/api/sql", q("SELECT text FROM workflow_run_events WHERE kind='input'"), sameOriginFetch, asBob); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Fatalf("another account read input history: %d %s", rec.Code, rec.Body)
	}
}

func TestWorkflowRunInputCannotReachAReassignedHost(t *testing.T) {
	h, _ := dbServer(t)
	ts := h.(*testServer)
	host := sendReport(t, h, wfReport())
	run := startRun(t, h, fixedWorkflow(t, h), "Improve the help page")
	succeed(t, host, host.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	host.conn.CloseNow()
	if err := ts.store.RemoveHost(t.Context(), ts.acct.ID, "host-1"); err != nil {
		t.Fatal(err)
	}
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	otherHost, _, err := dialStream(t, serve(t, h), "host-1", joinHost(t, ts.store, bob, "host-1"))
	if err != nil {
		t.Fatal(err)
	}
	report := wfReport()
	if ack := otherHost.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &report}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/input", `{"text":"Include keyboard shortcuts."}`, sameOriginFetch); rec.Code != http.StatusConflict {
		t.Fatalf("input to reassigned host = %d %s", rec.Code, rec.Body)
	}
}

func TestWorkflowRunInputPage(t *testing.T) {
	h, d := dbServer(t)
	host := sendReport(t, h, wfReport())
	run := startRun(t, h, fixedWorkflow(t, h), "Improve the help page")
	pageCheck := func(present []string, absent ...string) {
		t.Helper()
		rec := get(t, h, "/workflow-runs/"+run, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page = %d %s", rec.Code, rec.Body)
		}
		for _, want := range present {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("page misses %q", want)
			}
		}
		for _, unwanted := range absent {
			if strings.Contains(rec.Body.String(), unwanted) {
				t.Errorf("page includes %q", unwanted)
			}
		}
	}
	const composer = `id="additional-input"`
	const reply = `id="reply"`
	pageCheck(nil, composer, reply)
	succeed(t, host, host.command(), `{"workspaceId":"w-run","agentId":"orch-1"}`)
	pageCheck([]string{composer, `data-run-action="input"`}, reply)
	if page := get(t, h, "/workflow-runs/"+run, nil).Body.String(); strings.Count(page, composer) != 1 {
		t.Fatal("running run should have one input box")
	}

	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/input", `{"text":"Include shortcuts. <script>demo()</script>"}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("input = %d %s", rec.Code, rec.Body)
	}
	cmd := host.command()
	const safeText = "Include shortcuts. &lt;script&gt;demo()&lt;/script&gt;"
	pageCheck([]string{safeText, "Delivery pending"}, "<script>demo()")
	if _, err := d.Owner.Exec(t.Context(), "UPDATE commands SET expires_at=now()-interval '1 minute' WHERE id=$1", cmd.ID); err != nil {
		t.Fatal(err)
	}
	pageCheck([]string{safeText, "Delivery failed"}, "Delivery pending")
	succeed(t, host, cmd, `{}`)
	pageCheck([]string{safeText, "Delivered to orchestrator"}, "Delivery failed")

	if code, msg := move(t, h, keyOf(t, h, "host-1"), run, map[string]string{"move": "ask", "note": "Which shortcuts?"}); code != http.StatusOK {
		t.Fatalf("ask = %d %s", code, msg)
	}
	pageCheck([]string{reply, "Which shortcuts?", safeText}, composer)
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	pageCheck([]string{safeText, "Delivered to orchestrator"}, composer, reply)
}
