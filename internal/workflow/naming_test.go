package workflow

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// paseoBranch is the branch names the Paseo daemon accepts for a worktree (validateBranchSlug in
// @getpaseo/protocol): lowercase ASCII letters, digits, hyphens and slashes, no "--", at most 100.
var paseoBranch = regexp.MustCompile(`^[a-z0-9/]+(-[a-z0-9/]+)*$`)

func TestRunNamesSayWhichWorkflowAndWhatTask(t *testing.T) {
	const id = "b7ea7838-1c2d-4e5f-8a9b-0c1d2e3f4a5b" // its first 12 hex digits: b7ea78381c2d
	for _, c := range []struct {
		name, workflow, input, title, branch string
	}{
		{"keeps the words, drops articles", "Factory Task",
			"For workflows, automatically name the workspaces and the branches with good unique names that reflect the task.",
			"Factory Task: For workflows automatically name workspaces and branches · b7ea7838",
			"wf-factory-task-for-workflows-automatically-name-b7ea78381c2d"},
		{"keeps numbers and capitals", "Task from Intelligence",
			"How much subsidy do you get of Opus 5.5 on the Antigravity plan?\n\nDetails below.",
			"Task from Intelligence: How much subsidy do you get of Opus · b7ea7838",
			"wf-task-from-intelligence-how-much-subsidy-do-you-get-of-opus-b7ea78381c2d"},

		// negations survive, so the name never says the opposite of the task
		{"do not", "Factory Task", "Do not delete production data",
			"Factory Task: Do not delete production data · b7ea7838", "wf-factory-task-do-not-delete-production-data-b7ea78381c2d"},
		{"don't", "Factory Task", "Don't delete production data",
			"Factory Task: Don't delete production data · b7ea7838", "wf-factory-task-dont-delete-production-data-b7ea78381c2d"},
		{"make sure no", "Factory Task", "Make sure no emails are sent",
			"Factory Task: Make sure no emails are sent · b7ea7838", "wf-factory-task-make-sure-no-emails-are-sent-b7ea78381c2d"},
		{"polite never", "Factory Task", "Could you please never push to main without tests?",
			"Factory Task: Never push to main without tests · b7ea7838", "wf-factory-task-never-push-to-main-without-tests-b7ea78381c2d"},

		// the task is read from the whole input, past greetings, pleasantries and headings
		{"long polite preamble", "Factory Task",
			"Hi there! I hope your week is going well, and I appreciate all the help you have given me lately on all sorts of things. Could you please fix the login page?",
			"Factory Task: Fix login page · b7ea7838", "wf-factory-task-fix-login-page-b7ea78381c2d"},
		{"greeting in the sentence", "Factory Task", "Hey, can you fix the login page when you get a chance",
			"Factory Task: Fix login page when you get chance · b7ea7838", "wf-factory-task-fix-login-page-when-you-get-chance-b7ea78381c2d"},
		{"a heading line", "Factory Task", "Task:\n\nFix the flaky deploy check\n\nIt fails one run in ten.",
			"Factory Task: Fix flaky deploy check · b7ea7838", "wf-factory-task-fix-flaky-deploy-check-b7ea78381c2d"},
		{"a markdown heading", "Factory Task", "## Task\n- Fix the flaky deploy check",
			"Factory Task: Fix flaky deploy check · b7ea7838", "wf-factory-task-fix-flaky-deploy-check-b7ea78381c2d"},
		{"a label on the line", "Factory Task", `TASK (from Zach): "Show each agent's model in the tree."`,
			"Factory Task: Show each agent's model in tree · b7ea7838", "wf-factory-task-show-each-agents-model-in-tree-b7ea78381c2d"},

		// limits cut at whole words, and never end on a connecting word
		{"a long sentence", "Factory Task",
			"Add a button to the settings page that exports every account's hosts as CSV for auditing",
			"Factory Task: Add button to settings page that exports every · b7ea7838",
			"wf-factory-task-add-button-to-settings-page-that-exports-b7ea78381c2d"},
		{"a long workflow name", "The Very Long Workflow Name That Goes On And On", "Fix the login page",
			"The Very Long Workflow Name That Goes On And On: Fix login page · b7ea7838",
			"wf-the-very-long-workflow-fix-login-page-b7ea78381c2d"},
		{"one very long word", "Factory Task", "Supercalifragilisticexpialidociousandthensomemoreletters now",
			"Factory Task: Supercalifragilisticexpialidociousandthensomemoreletters now · b7ea7838",
			"wf-factory-task-b7ea78381c2d"},

		// the title keeps any script; the branch keeps ASCII (Paseo accepts nothing else), folding accents
		{"accents", "Tâches", "Café: añadir los platos del día",
			"Tâches: Café añadir los platos del día · b7ea7838", "wf-taches-cafe-anadir-los-platos-del-dia-b7ea78381c2d"},
		{"no ASCII task", "Factory Task", "修复登录页面",
			"Factory Task: 修复登录页面 · b7ea7838", "wf-factory-task-b7ea78381c2d"},
		{"no ASCII at all", "任务", "修复登录页面", "任务: 修复登录页面 · b7ea7838", "wf-b7ea78381c2d"},
		{"no workflow name", "", "Fix the login page", "Fix login page · b7ea7838", "wf-fix-login-page-b7ea78381c2d"},
	} {
		title, br := runNames(id, c.workflow, c.input)
		if title != c.title || br != c.branch {
			t.Errorf("%s: runNames(%q, %q)\n got  %q, %q\n want %q, %q", c.name, c.workflow, c.input, title, br, c.title, c.branch)
		}
		if len(br) > 100 || !paseoBranch.MatchString(br) {
			t.Errorf("%s: Paseo would refuse branch %q", c.name, br)
		}
		if err := exec.Command("git", "check-ref-format", "--branch", br).Run(); err != nil {
			t.Errorf("%s: branch %q is not a valid git branch: %v", c.name, br, err)
		}
	}
}

func TestRunNamesAreUniquePerRun(t *testing.T) {
	ta, ba := runNames("aaaaaaaa-1111-4000-8000-000000000000", "Factory Task", "Fix the login page")
	_, bb := runNames("aaaaaaaa-2222-4000-8000-000000000000", "Factory Task", "Fix the login page")
	if ba == bb || !strings.HasSuffix(ba, "-aaaaaaaa1111") {
		t.Errorf("branches %q and %q", ba, bb)
	}
	tc, _ := runNames("bbbbbbbb-1111-4000-8000-000000000000", "Factory Task", "Fix the login page")
	if ta == tc || !strings.HasSuffix(ta, " · aaaaaaaa") {
		t.Errorf("titles %q and %q", ta, tc)
	}
}
