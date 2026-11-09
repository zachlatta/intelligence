package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTheStartingWorkflowIsValid(t *testing.T) {
	if err := Starting.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Starting)
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
	ok := func() Definition { return clone(Starting) }
	for name, c := range map[string]struct {
		mod  func(*Definition)
		want string
	}{
		"no steps":           {func(d *Definition) { d.Steps = nil }, "at least one step"},
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
