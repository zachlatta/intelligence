package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTheStartingWorkflowIsValidOnceItHasAHost(t *testing.T) {
	if err := Starting.Validate(); err == nil || !strings.Contains(err.Error(), "host") {
		t.Errorf("the starting workflow names no host: err = %v", err)
	}
	b, err := json.Marshal(onHost(Starting))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse(Starting) = %v", err)
	}
	if len(d.Steps) != len(Starting.Steps) || d.Steps[0].ID != "build" {
		t.Errorf("round trip = %+v", d)
	}
}

func TestParseRefusesWhatCannotRun(t *testing.T) {
	ok := func() Definition { return onHost(clone(Starting)) }
	for name, c := range map[string]struct {
		mod  func(*Definition)
		want string
	}{
		"no steps":           {func(d *Definition) { d.Steps = nil }, "at least one step"},
		"no host":            {func(d *Definition) { d.Host = "" }, "host"},
		"bad host":           {func(d *Definition) { d.Host = "Not A Host" }, "host"},
		"unknown trigger":    {func(d *Definition) { d.Trigger = "email" }, "trigger"},
		"unknown kind":       {func(d *Definition) { d.Steps[0].Kind = "decide" }, "kind"},
		"bad step id":        {func(d *Definition) { d.Steps[0].ID = "Do It" }, "id"},
		"repeated id":        {func(d *Definition) { d.Steps[1].ID = d.Steps[0].ID }, "twice"},
		"no title":           {func(d *Definition) { d.Steps[0].Title = " " }, "title"},
		"no instructions":    {func(d *Definition) { d.Steps[0].Instructions = "" }, "instructions"},
		"unknown mode":       {func(d *Definition) { d.Project.Mode = "guess" }, "mode"},
		"fixed, no dir":      {func(d *Definition) { d.Project.Mode = "fixed"; d.Project.Directory = "" }, "directory"},
		"fixed, relative":    {func(d *Definition) { d.Project.Mode = "fixed"; d.Project.Directory = "dev/app" }, "directory"},
		"no orchestrator":    {func(d *Definition) { d.Orchestrator.Provider = "" }, "provider"},
		"no project agent":   {func(d *Definition) { d.Project.Provider = "" }, "provider"},
		"orchestrator blank": {func(d *Definition) { d.Orchestrator.Instructions = "" }, "instructions"},
	} {
		d := ok()
		c.mod(&d)
		b, _ := json.Marshal(d)
		if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, c.want)
		}
	}
	if _, err := Parse([]byte(`{"trigger":"task","surprise":1}`)); err == nil {
		t.Error("an unknown field should be refused")
	}
	fixed := ok()
	fixed.Project = Project{Mode: "fixed", Directory: "/home/someone/dev/app"}
	b, _ := json.Marshal(fixed)
	if _, err := Parse(b); err != nil {
		t.Errorf("a fixed project needs no agent: %v", err)
	}
}

func TestTheProjectFinderIsToldTheRepositoriesAndHowToAnswer(t *testing.T) {
	p := ProjectPrompt("run-1", Starting, "Show each agent's model", []string{"/srv/dev/app", "/srv/dev/site"})
	for _, want := range []string{Starting.Project.Instructions, "Show each agent's model", "- /srv/dev/app\n- /srv/dev/site",
		"intelligence workflow project --run run-1 <directory>", "intelligence workflow project --run run-1 none"} {
		if !strings.Contains(p, want) {
			t.Errorf("project prompt misses %q:\n%s", want, p)
		}
	}
}

func TestTheOrchestratorGetsTheRunTheStepsAndTheFirstStep(t *testing.T) {
	vars := Vars{Run: "run-1", Input: "Show each agent's model", Repo: "/srv/dev/app", Branch: "wf-run1"}
	p := OrchestratorPrompt("Task from Intelligence", Starting, vars)
	for _, want := range []string{Starting.Orchestrator.Instructions, "run run-1 of the workflow “Task from Intelligence”",
		"Show each agent's model", "1. Do the work (step, id build)", "5. Zach's sign-off (signoff, id signoff)",
		"intelligence workflow next --run run-1", "intelligence workflow back --run run-1 <step id>",
		"intelligence workflow ask --run run-1 --note", "Step 1 of 6: Do the work"} {
		if !strings.Contains(p, want) {
			t.Errorf("orchestrator prompt misses %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "{{") {
		t.Errorf("a variable was left unfilled:\n%s", p)
	}
}

func TestAStepsInstructionsFillItsVariables(t *testing.T) {
	d := clone(Starting)
	d.Steps[0].Instructions = "Do {{input}} in {{repo}} on {{branch}} for run {{run}}; leave {{unknown}}."
	got := StepMessage(d, 0, Vars{Run: "r", Input: "it", Repo: "/a", Branch: "b"})
	if !strings.Contains(got, "Step 1 of 6: Do the work") || !strings.Contains(got, "Do it in /a on b for run r; leave {{unknown}}.") {
		t.Errorf("message = %q", got)
	}
}

func clone(d Definition) Definition {
	b, _ := json.Marshal(d)
	var out Definition
	_ = json.Unmarshal(b, &out)
	return out
}

func onHost(d Definition) Definition {
	d.Host = "host-1"
	return d
}

func TestWorkflowAgentsStartInTheModeThatNeverAsks(t *testing.T) {
	// The ids are the modes paseo's provider catalogs mark isUnattended.
	for provider, want := range map[string]string{"claude": "bypassPermissions", "codex": "full-access",
		"muse": "allowAll", "antigravity": "full-access", "other": ""} {
		if got := agentMode(provider); got != want {
			t.Errorf("agentMode(%q) = %q, want %q", provider, got, want)
		}
	}
	// Providers is what the editor offers: each has a mode that never asks, Claude first.
	ps := Providers()
	if len(ps) != 4 || ps[0] != "claude" {
		t.Errorf("Providers() = %v", ps)
	}
	for _, p := range ps {
		if agentMode(p) == "" {
			t.Errorf("provider %q has no mode that never asks", p)
		}
	}
}

func TestTheOrchestratorStartsSubAgentsInTheModeThatNeverAsks(t *testing.T) {
	if !strings.Contains(OrchestratorInstructions, "--provider claude/claude-opus-5-5 --mode "+agentMode("claude")) {
		t.Errorf("the example sub-agent has no mode:\n%s", OrchestratorInstructions)
	}
	// A definition saved before the modes were in its instructions still gets them: the prompt says them.
	d := clone(Starting)
	d.Orchestrator.Instructions = "You run a workflow. Start sub-agents with paseo run."
	p := OrchestratorPrompt("Task", d, Vars{Run: "run-1", Input: "x"})
	for _, want := range []string{"--mode bypassPermissions for claude", "--mode full-access for codex", "--mode allowAll for muse", "--mode full-access for antigravity"} {
		if !strings.Contains(p, want) {
			t.Errorf("orchestrator prompt misses %q:\n%s", want, p)
		}
	}
}

// Saved workflows can have older standing instructions. The generated prompt must still
// tell the orchestrator to pass the current step's limits to workers.
func TestSavedWorkflowsKeepWorkersInsideTheirAssignedStep(t *testing.T) {
	d := clone(Starting)
	d.Orchestrator.Instructions = "Coordinate this task."
	p := OrchestratorPrompt("Saved workflow", d, Vars{Run: "run-1"})
	for _, want := range []string{
		"Workflow instructions take precedence over conflicting shared skill defaults",
		"Pass each sub-agent its role, current step, run id, task context and limits",
		"Only the orchestrator moves the run",
		"Builders may edit, test and commit in the worktree",
		"do not push, land, deploy or change external systems before the step that authorizes it",
		"Follow this workflow's model choices",
		"pass proposed edits and evidence to the landing step",
		"After a successful ask, end your turn",
		"his reply comes as your next message",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("saved workflow prompt misses %q", want)
		}
	}
}

func TestLandingOwnsApprovedExternalChanges(t *testing.T) {
	const want = "The landing step also applies the run's approved external changes (skill edits, queued mutations)."
	// Saved definitions get the contract too; neither their standing instructions nor
	// their landing step needs to have been updated to the new defaults.
	d := clone(Starting)
	d.Orchestrator.Instructions = "Coordinate this task."
	d.Steps[d.Index("land")].Instructions = "Land on main and wait for the deploy."
	if p := OrchestratorPrompt("Saved workflow", d, Vars{Run: "run-1"}); !strings.Contains(p, want) {
		t.Errorf("saved workflow does not assign approved external changes to landing")
	}
	if p := StepMessage(Starting, Starting.Index("land"), Vars{Run: "run-1"}); !strings.Contains(p, want) {
		t.Errorf("landing step does not own approved external changes")
	}
}

func TestWorkflowPromptsAvoidPermissionTraps(t *testing.T) {
	for _, p := range []string{OrchestratorPrompt("task", Starting, Vars{Run: "r"}), StepMessage(Starting, 0, Vars{})} {
		for _, want := range []string{"Write/Edit", "heredoc", "--mode bypassPermissions", "--mode full-access"} {
			if !strings.Contains(p, want) {
				t.Errorf("prompt misses %q", want)
			}
		}
	}
}
