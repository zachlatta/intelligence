package workflow

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRunNamesSayWhichWorkflowAndWhatTask(t *testing.T) {
	const id = "b7ea7838-0000-4000-8000-000000000000"
	for _, c := range []struct {
		workflow, input, title, branch string
	}{
		{"Factory Task",
			"For workflows, automatically name the workspaces and the branches with good unique names that reflect the task.",
			"Factory Task: Workflows automatically name workspaces branches",
			"wf-factory-task-workflows-automatically-name-workspaces-b7ea7838"},
		{"Factory Task",
			"in workflows in intelligence let me pick a default workflow so it is first on the list",
			"Factory Task: Workflows intelligence pick default first",
			"wf-factory-task-workflows-intelligence-pick-default-b7ea7838"},
		{"Task from Intelligence",
			"How much subsidy do you get of Opus 5.5 on the Antigravity plan?\n\nDetails below.",
			"Task from Intelligence: Subsidy Opus 5.5 Antigravity plan",
			"wf-task-from-intelligence-subsidy-opus-5-5-antigravity-plan-b7ea7838"},
		// nothing but filler: the first line stands in
		{"Research", "Can you do it?", "Research: Can you do it?", "wf-research-can-you-do-it-b7ea7838"},
		// no workflow name and no ASCII: still a valid, unique branch
		{"", "日本語のタスク", "日本語のタスク", "wf-b7ea7838"},
		// a very long workflow name is cut at a word
		{"The Very Long Workflow Name That Goes On And On", "Fix the login page",
			"The Very Long Workflow Name That Goes On And On: Fix login page",
			"wf-the-very-long-workflow-fix-login-page-b7ea7838"},
	} {
		title, br := runNames(id, c.workflow, c.input)
		if title != c.title || br != c.branch {
			t.Errorf("runNames(%q, %q) = %q, %q; want %q, %q", c.workflow, c.input, title, br, c.title, c.branch)
		}
		if len(br) > 80 {
			t.Errorf("branch %q is %d characters", br, len(br))
		}
		if err := exec.Command("git", "check-ref-format", "--branch", br).Run(); err != nil {
			t.Errorf("branch %q is not a valid git branch: %v", br, err)
		}
	}
}

func TestRunNamesAreUniquePerRun(t *testing.T) {
	_, a := runNames("aaaaaaaa-0000-4000-8000-000000000000", "Factory Task", "Fix the login page")
	_, b := runNames("bbbbbbbb-0000-4000-8000-000000000000", "Factory Task", "Fix the login page")
	if a == b || !strings.HasSuffix(a, "-aaaaaaaa") {
		t.Errorf("branches %q and %q", a, b)
	}
}
