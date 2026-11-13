package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// startedOrchestrator reads the command that starts run's orchestrator off b, reports it started
// as agent, and returns its prompt.
func startedOrchestrator(t *testing.T, b *rawHost, agent string) string {
	t.Helper()
	ws := b.command()
	var wa hostclient.CreateWorkspaceArgs
	json.Unmarshal(ws.Args, &wa)
	if ws.Kind != hostclient.CreateWorkspaceKind || wa.Agent == nil {
		t.Fatalf("want the orchestrator's workspace, got %s %s", ws.Kind, ws.Args)
	}
	succeed(t, b, ws, `{"workspaceId":"w-`+agent+`","agentId":"`+agent+`"}`)
	return wa.Agent.Prompt
}

// The 2026-11-12 case, replayed: an email run rebuilds a board deck with a grant map,
// and two minutes later a voice memo asks for quote slides in the same deck. The second run must
// find the first and steer into it, not rebuild and republish the deck in parallel.
func TestASecondRunOnTheSameDeliverableSteersIntoTheFirst(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")

	emailDef := workflow.Starting
	emailDef.Trigger = workflow.TriggerEmail
	emailDef.Email = &workflow.EmailFilter{From: []string{"requester@example.com"}}
	emailDef.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	emailWF := createWorkflow(t, h, emailDef)
	voiceDef := workflow.Starting
	voiceDef.Trigger = workflow.TriggerVoiceMemo
	voiceDef.VoiceMemo = &workflow.VoiceMemoFilter{}
	voiceDef.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	voiceWF := createWorkflow(t, h, voiceDef)

	deck := "https://files.example.com/0b7e1f6a-deck/index.html"
	mapRun := startRun(t, h, emailWF, (workflow.Mail{MessageID: "m-map", ThreadID: "t-map", From: "requester@example.com",
		Subject: "Re: Grant map for the board deck", Text: "Go ahead with what you have."}).Input(true))
	first := startedOrchestrator(t, b, "orch-map")
	if !strings.Contains(first, "intelligence workflow claim --run "+mapRun) || !strings.Contains(first, "No other run is live") {
		t.Fatalf("the first run is not told to claim its deliverable before building:\n%s", first)
	}
	if code, msg := move(t, h, key, mapRun, map[string]string{"move": "summarize", "summary": "Build grant map for board deck"}); code != http.StatusOK {
		t.Fatalf("summarize: %d %s", code, msg)
	}
	if code, msg := move(t, h, key, mapRun, map[string]string{"move": "claim", "deliverable": deck}); code != http.StatusOK || !strings.Contains(msg, "expected_version") {
		t.Fatalf("claim: %d %s", code, msg)
	}

	quoteRun := startRun(t, h, voiceWF, (workflow.VoiceMemo{Source: "recorder", Account: "owner", ID: "memo-quotes",
		Title:      "Board deck: partner quotes",
		Transcript: "In the board deck, add a slide of partner quotes."}).Input())
	second := startedOrchestrator(t, b, "orch-quotes")
	for _, want := range []string{mapRun, "Build grant map for board deck", "https://files.example.com/0b7e1f6a-deck",
		"orch-map", "intelligence workflow steer --run " + quoteRun + " --into <run id>"} {
		if !strings.Contains(second, want) {
			t.Fatalf("the second run's prompt does not list the live run (%q):\n%s", want, second)
		}
	}

	// It works out the deck it would change and claims it before building: refused, naming the
	// run that holds it. A folder and its index.html are one deliverable; so is an asset in it.
	for _, claim := range []string{"https://FILES.example.com/0b7e1f6a-deck/", "https://files.example.com/0b7e1f6a-deck/map.png?v=2"} {
		code, msg := move(t, h, key, quoteRun, map[string]string{"move": "claim", "deliverable": claim})
		if code != http.StatusConflict || !strings.Contains(msg, mapRun) || !strings.Contains(msg, "intelligence workflow steer --run "+quoteRun+" --into "+mapRun) {
			t.Fatalf("claim of %s: %d %s", claim, code, msg)
		}
	}
	if code, msg := move(t, h, key, quoteRun, map[string]string{"move": "steer", "into": quoteRun, "note": "x"}); code != http.StatusBadRequest {
		t.Fatalf("steer into itself: %d %s", code, msg)
	}
	note := "The requester asks for two quote slides in this deck: partners and sponsors."
	code, msg := move(t, h, key, quoteRun, map[string]string{"move": "steer", "into": mapRun, "note": note, "deliverable": deck})
	if code != http.StatusOK || !strings.Contains(msg, "finished") {
		t.Fatalf("steer: %d %s", code, msg)
	}
	sent := b.command()
	var ma hostclient.AgentArgs
	json.Unmarshal(sent.Args, &ma)
	if sent.Kind != hostclient.SendMessageKind || ma.AgentID != "orch-map" || !strings.Contains(ma.Text, note) ||
		!strings.Contains(ma.Text, "memo-quotes") || !strings.Contains(ma.Text, quoteRun) {
		t.Fatalf("the steer did not reach the deck run's orchestrator: %s %s", sent.Kind, sent.Args)
	}
	if c := b.command(); c.Kind != hostclient.ArchiveWorkspaceKind {
		t.Fatalf("the steered run's workspace should be archived: %s", c.Kind)
	}
	if r := readRun(t, h, quoteRun); r["status"] != "succeeded" {
		t.Fatalf("the steered run should be over: %v", r)
	}
	if r := readRun(t, h, mapRun); r["status"] != "running" {
		t.Fatalf("the deck run should carry on: %v", r)
	}
	// the steer is the steered run's answer resolution: the deck run answers it
	code, steers := sqlPost(t, h, q(`SELECT run_id::text, kind, steered_to::text, reason FROM workflow_run_answer_resolutions`))
	if code != http.StatusOK || len(steers.Rows) != 1 || steers.Rows[0][0] != quoteRun || steers.Rows[0][1] != "steered" || steers.Rows[0][2] != mapRun ||
		steers.Rows[0][3] != "https://files.example.com/0b7e1f6a-deck: "+note {
		t.Fatalf("steer record: %d %+v", code, steers)
	}
	code, events := sqlPost(t, h, q(`SELECT run_id::text, kind FROM workflow_run_events WHERE kind IN ('claim','steer') OR text LIKE '%steered into this run%' ORDER BY id`))
	if code != http.StatusOK || len(events.Rows) != 3 || events.Rows[1][0] != mapRun || events.Rows[1][1] != "message" || events.Rows[2][0] != quoteRun || events.Rows[2][1] != "steer" {
		t.Fatalf("events: %+v", events)
	}
	if page := get(t, h, "/workflow-runs/"+quoteRun, nil).Body.String(); !strings.Contains(page, `Steered into run <a href="/workflow-runs/`+mapRun+`">`) ||
		!strings.Contains(page, "Steered to run "+mapRun[:8]+"; awaiting its answer") || strings.Contains(page, "No answer recorded") {
		t.Fatal("the steered run's page does not say where its work and its answer went")
	}
	if page := get(t, h, "/workflow-runs/"+mapRun, nil).Body.String(); !strings.Contains(page, "Claimed <a href=\"https://files.example.com/0b7e1f6a-deck\"") ||
		!strings.Contains(page, `Run <a href="/workflow-runs/`+quoteRun+`">`) || !strings.Contains(page, "steered into this run") {
		t.Fatal("the deck run's page does not show its claim and the steer")
	}
	// a steered run's answer comes from the run it steered into
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET requested_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	code, unanswered := sqlPost(t, h, q(`SELECT id::text, coalesce(steered_to::text, '') FROM workflow_runs_unanswered ORDER BY steered_to NULLS FIRST`))
	if code != http.StatusOK || len(unanswered.Rows) != 2 || unanswered.Rows[0][0] != mapRun || unanswered.Rows[1][0] != quoteRun || unanswered.Rows[1][1] != mapRun {
		t.Fatalf("unanswered: %+v", unanswered)
	}

	// once the deck run is over, its claim no longer holds anything
	if code, msg := move(t, h, key, mapRun, map[string]string{"move": "done", "summary": "deck sent"}); code != http.StatusOK {
		t.Fatalf("done: %d %s", code, msg)
	}
	b.command() // archive
	later := startRun(t, h, voiceWF, "Voice memo: one more deck change")
	startedOrchestrator(t, b, "orch-later")
	if code, msg := move(t, h, key, later, map[string]string{"move": "claim", "deliverable": deck}); code != http.StatusOK {
		t.Fatalf("claim after the holder finished: %d %s", code, msg)
	}
	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

// A run steered into another hands over its thread: a later email in that thread goes to the run
// it steered into while that run is live.
func TestAReplyInASteeredRunsThreadGoesToTheRunItSteeredInto(t *testing.T) {
	h, fake := emailServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	christina(t, h)
	connectGmail(t, h, fake)

	fake.Add(mailMsg("d1", "t-deck", "christina@example.com", "Build the board deck", "Please build it."))
	deckRun, _ := decided(t, h, "d1")["run_id"].(string)
	startedOrchestrator(t, b, "orch-deck")
	fake.Add(mailMsg("q1", "t-quotes", "christina@example.com", "Quotes for the board deck", "Add funder quotes."))
	quoteRun, _ := decided(t, h, "q1")["run_id"].(string)
	// the thread chain is checkable before building: the live list names each email run's thread
	if p := startedOrchestrator(t, b, "orch-quotes"); !strings.Contains(p, "email: “Build the board deck” (thread t-deck)") {
		t.Fatalf("the live run's email thread is missing from the prompt:\n%s", p)
	}
	if deckRun == "" || quoteRun == "" || deckRun == quoteRun {
		t.Fatalf("runs: %q %q", deckRun, quoteRun)
	}
	if code, msg := move(t, h, key, quoteRun, map[string]string{"move": "steer", "into": deckRun, "note": "Add funder quotes to the deck."}); code != http.StatusOK {
		t.Fatalf("steer: %d %s", code, msg)
	}
	b.command() // the steer's message
	b.command() // archive of the steered run

	fake.Add(mailMsg("q2", "t-quotes", "christina@example.com", "Re: Quotes for the board deck", "Use the sponsor quote."))
	if r := decided(t, h, "q2"); r["decision"] != "added" || r["run_id"] != deckRun {
		t.Fatalf("q2 = %v", r)
	}
	msg := b.command()
	var ma hostclient.AgentArgs
	json.Unmarshal(msg.Args, &ma)
	if msg.Kind != hostclient.SendMessageKind || ma.AgentID != "orch-deck" || !strings.Contains(ma.Text, "sponsor quote") {
		t.Fatalf("message = %s %s", msg.Kind, msg.Args)
	}
}

// A steered run is answered only through its target, and only by what the target sends from the
// handover on: while the target is live the steered run waits on it; once it answers, the steered
// run is answered; once it is over without such an answer, the steered run is unanswered again and
// says why. An answer the target sent before the steer (the 2026-11-12 shape) answers only the
// target's own request. Steering into a run that itself steered goes on to the live run.
func TestASteeredRunIsAnsweredOnlyThroughItsTarget(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	voiceDef := workflow.Starting
	voiceDef.Trigger = workflow.TriggerVoiceMemo
	voiceDef.VoiceMemo = &workflow.VoiceMemoFilter{}
	voiceDef.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	wf := createWorkflow(t, h, voiceDef)
	runs := map[string]string{}
	for _, name := range []string{"t1", "s1", "t2", "s2", "t3", "s3", "t4", "s4", "c"} {
		runs[name] = startRun(t, h, wf, "Voice memo: deck change "+name)
		startedOrchestrator(t, b, "orch-"+name)
	}
	answer := func(name, id, sentAt string) {
		t.Helper()
		body := `{"channel":"email","messageId":"` + id + `","checked":true` + sentAt + `}`
		if rec := call(t, anon(h), "POST", "/api/workflow-runs/"+runs[name]+"/answers", body, map[string]string{"Authorization": "Bearer " + key}); rec.Code != http.StatusOK {
			t.Fatalf("answer %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// t4 answered its own request before s4 steered to it
	answer("t4", "early", `,"sentAt":"2026-01-01T00:00:00Z"`)
	steer := func(from, into string) hostclient.AgentArgs {
		t.Helper()
		if code, msg := move(t, h, key, runs[from], map[string]string{"move": "steer", "into": runs[into], "note": "Same deck: " + from}); code != http.StatusOK {
			t.Fatalf("steer %s into %s: %d %s", from, into, code, msg)
		}
		var ma hostclient.AgentArgs
		json.Unmarshal(b.command().Args, &ma)
		b.command() // archive
		return ma
	}
	for _, p := range [][2]string{{"s1", "t1"}, {"s2", "t2"}, {"s3", "t3"}, {"s4", "t4"}} {
		steer(p[0], p[1])
	}
	// c steers into s1, which already steered into t1: it reaches t1
	if ma := steer("c", "s1"); ma.AgentID != "orch-t1" {
		t.Fatalf("steering into a steered run should reach the live run it steered into, got %s", ma.AgentID)
	}
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET requested_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	unanswered := func() map[string]bool {
		t.Helper()
		code, r := sqlPost(t, h, q(`SELECT id::text FROM workflow_runs_unanswered`))
		if code != http.StatusOK {
			t.Fatalf("unanswered: %d", code)
		}
		out := map[string]bool{}
		for name, id := range runs {
			for _, row := range r.Rows {
				if row[0] == id {
					out[name] = true
				}
			}
		}
		return out
	}
	page := func(name string) string { return get(t, h, "/workflow-runs/"+runs[name], nil).Body.String() }
	waiting := func(name, on string) {
		t.Helper()
		p := page(name)
		if !strings.Contains(p, "Steered to run "+runs[on][:8]+"; awaiting its answer") || strings.Contains(p, "No answer recorded") || strings.Contains(p, "answered:") {
			t.Fatalf("%s should be waiting on %s", name, on)
		}
	}
	missing := func(name string) {
		t.Helper()
		p := page(name)
		if !strings.Contains(p, "No answer recorded") || strings.Contains(p, "answered:") || strings.Contains(p, "awaiting its answer") {
			t.Fatalf("%s should be unanswered", name)
		}
	}

	// targets live: steered runs wait on them; t4's earlier answer does not answer s4
	if u := unanswered(); !u["s1"] || !u["s4"] || !u["c"] || !u["t1"] || u["t4"] {
		t.Fatalf("while the targets are live: %v", u)
	}
	waiting("s1", "t1")
	waiting("s4", "t4")
	if p := page("t1"); !strings.Contains(p, `href="/workflow-runs/`+runs["s1"]+`"`) {
		t.Fatal("run ids in steer lines should link to the run")
	}

	// t1 answers; t2 is canceled and t3 and t4 finish, none with an answer since the steer
	answer("t1", "sent-1", "")
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runs["t2"]+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{"t3", "t4"} {
		if code, msg := move(t, h, key, runs[name], map[string]string{"move": "done", "summary": "done"}); code != http.StatusOK {
			t.Fatalf("done: %d %s", code, msg)
		}
	}
	if u := unanswered(); u["s1"] || u["c"] || u["t1"] || !u["s2"] || !u["s3"] || !u["s4"] || !u["t3"] || u["t4"] {
		t.Fatalf("after the targets: %v", u)
	}
	for _, name := range []string{"s1", "c"} {
		if p := page(name); !strings.Contains(p, "(via run "+runs["t1"][:8]+")") || strings.Contains(p, "No answer recorded") {
			t.Fatalf("%s should be answered through t1", name)
		}
	}
	for _, name := range []string{"s2", "s3", "s4"} {
		missing(name)
	}
	code, ev := sqlPost(t, h, q(`SELECT run_id::text FROM workflow_run_events WHERE kind='steer_unanswered'`))
	if code != http.StatusOK || len(ev.Rows) != 3 {
		t.Fatalf("each steered run left unanswered gets an event: %+v", ev)
	}
	if p := page("s2"); !strings.Contains(p, "ended (canceled) without a checked answer") {
		t.Fatal("the steered run's page does not say its target ended without answering")
	}
}
