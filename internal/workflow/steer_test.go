package workflow

import (
	"strings"
	"testing"
	"time"
)

func TestDeliverableKeysCompareOneThingInEveryForm(t *testing.T) {
	for in, want := range map[string]string{
		"https://Files.Example.com/ns/index.html":    "https://files.example.com/ns",
		"https://files.example.com/ns/":              "https://files.example.com/ns",
		"https://files.example.com/ns/map.png?v=2#x": "https://files.example.com/ns/map.png",
		"https://files.example.com/ns/../ns/deck":    "https://files.example.com/ns/deck",
		"  Board   Meeting DECK ":                    "board meeting deck",
	} {
		if got, err := DeliverableKey(in); err != nil || got != want {
			t.Errorf("DeliverableKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "two\nlines", strings.Repeat("x", 501)} {
		if _, err := DeliverableKey(in); err == nil {
			t.Errorf("DeliverableKey(%q) accepted", in)
		}
	}
}

func TestTheCLIClaimsAndSteers(t *testing.T) {
	run := "11111111-1111-4111-8111-111111111111"
	m, got, err := parseCLI([]string{"claim", "--run", run, "https://files.example.com/ns/"}, nil)
	if err != nil || got != run || m.Move != "claim" || m.Deliverable != "https://files.example.com/ns/" {
		t.Fatalf("claim: %+v %v", m, err)
	}
	m, _, err = parseCLI([]string{"steer", "--run", run, "--into", "22222222-2222-4222-8222-222222222222", "--deliverable", "deck", "--note", "-"}, strings.NewReader("Add quote slides."))
	if err != nil || m.Into != "22222222-2222-4222-8222-222222222222" || m.Note != "Add quote slides." || m.Deliverable != "deck" {
		t.Fatalf("steer: %+v %v", m, err)
	}
	for _, args := range [][]string{{"claim", "--run", run}, {"steer", "--run", run, "--note", "-"}, {"steer", "--run", run, "--into", "x"}} {
		if _, _, err := parseCLI(args, strings.NewReader("note")); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestEmailAndVoiceMemoRunsCheckLiveRunsBeforeBuilding(t *testing.T) {
	d := Starting
	d.Trigger = TriggerVoiceMemo
	d.VoiceMemo = &VoiceMemoFilter{}
	live := []LiveRun{{ID: "run-deck", Workflow: "Zach Email Task", Summary: "Build grant map\nfor the deck", Status: Running, Orchestrator: "orch-1",
		Started: time.Date(2026, 11, 13, 3, 39, 0, 0, time.UTC), Claims: []string{"https://files.example.com/ns"},
		Emails: []string{"“Re: Grant map” (thread t-map)"}}}
	p := OrchestratorPrompt("Voice Memo Received", d, Vars{Run: "run-quotes", Input: "Voice memo: quotes", Live: &LiveRuns{Runs: live, Total: 1}})
	for _, want := range []string{"run-deck · “Zach Email Task” · Build grant map for the deck · running, step 1 · started 2026-11-13T03:39:00Z · orchestrator orch-1 · claims: https://files.example.com/ns",
		"intelligence workflow steer --run run-quotes --into <run id>", "email: “Re: Grant map” (thread t-map)", "intelligence workflow claim --run run-quotes <link or name>", "same email thread chain", "Claim the public link (or folder) as soon as you know it"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt misses %q", want)
		}
	}
	if strings.Index(p, "Before building anything") > strings.Index(p, "Step 1 of") {
		t.Error("the check should come before the first step")
	}
	// every step, of every workflow, carries the publishing guard
	step := StepMessage(Starting, 0, Vars{Run: "run-x"})
	for _, want := range []string{"intelligence workflow claim --run run-x", "expected_version", "new file name"} {
		if !strings.Contains(step, want) {
			t.Errorf("step misses %q", want)
		}
	}
	if task := OrchestratorPrompt("Factory Task", Starting, Vars{Run: "run-x"}); strings.Contains(task, "Before building anything") {
		t.Error("a task run has no live-run check")
	}
}

func TestATruncatedLiveRunListSaysHowToFindTheRest(t *testing.T) {
	live := LiveRuns{Runs: []LiveRun{{ID: "run-new", Workflow: "Zach Email Task", Summary: "Deck", Status: Running}}, Total: 31}
	p := liveRunsSection("run-x", live)
	for _, want := range []string{"These are the 1 newest of 31 live runs", LiveRunsQuery, "A claim is checked against every live run"} {
		if !strings.Contains(p, want) {
			t.Errorf("section misses %q:\n%s", want, p)
		}
	}
	if p := liveRunsSection("run-x", LiveRuns{Runs: live.Runs, Total: 1}); strings.Contains(p, "newest of") {
		t.Error("a complete list says it is truncated")
	}
}
