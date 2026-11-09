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
	const id = "b7ea7838-1c2d-4e5f-8a9b-0c1d2e3f4a5b" // its first 12 hex digits, which end both names: b7ea78381c2d
	for _, c := range []struct {
		name, workflow, input, title, branch string
	}{
		{"keeps the words", "Factory Task",
			"For workflows, automatically name the workspaces and the branches with good unique names that reflect the task.",
			"Factory Task: For workflows, automatically name the workspaces · b7ea78381c2d",
			"wf-factory-task-for-workflows-automatically-name-b7ea78381c2d"},
		{"keeps numbers and capitals", "Task from Intelligence",
			"How much subsidy do you get of Opus 5.5 on the Antigravity plan?\n\nDetails below.",
			"Task from Intelligence: How much subsidy do you get of Opus · b7ea78381c2d",
			"wf-task-from-intelligence-how-much-subsidy-do-you-get-of-opus-b7ea78381c2d"},

		// negations survive, so the name never says the opposite of the task
		{"do not", "Factory Task", "Do not delete production data",
			"Factory Task: Do not delete production data · b7ea78381c2d", "wf-factory-task-do-not-delete-production-data-b7ea78381c2d"},
		{"don't", "Factory Task", "Don't delete production data",
			"Factory Task: Don't delete production data · b7ea78381c2d", "wf-factory-task-dont-delete-production-data-b7ea78381c2d"},
		{"make sure no", "Factory Task", "Make sure no emails are sent",
			"Factory Task: Make sure no emails are sent · b7ea78381c2d", "wf-factory-task-make-sure-no-emails-are-sent-b7ea78381c2d"},
		{"polite never", "Factory Task", "Could you please never push to main without tests?",
			"Factory Task: Never push to main without tests · b7ea78381c2d", "wf-factory-task-never-push-to-main-without-tests-b7ea78381c2d"},

		// the task is read from the whole input, past greetings, pleasantries and headings
		{"long polite preamble", "Factory Task",
			"Hi there! I hope your week is going well. Thanks so much for all your help! Could you please fix the login page?",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"greeting in the sentence", "Factory Task", "Hey, can you fix the login page when you get a chance",
			"Factory Task: Fix the login page when you get · b7ea78381c2d", "wf-factory-task-fix-the-login-page-when-you-get-b7ea78381c2d"},
		{"a heading line", "Factory Task", "Task:\n\nFix the flaky deploy check\n\nIt fails one run in ten.",
			"Factory Task: Fix the flaky deploy check · b7ea78381c2d", "wf-factory-task-fix-the-flaky-deploy-check-b7ea78381c2d"},
		{"a markdown heading", "Factory Task", "## Task\n- Fix the flaky deploy check",
			"Factory Task: Fix the flaky deploy check · b7ea78381c2d", "wf-factory-task-fix-the-flaky-deploy-check-b7ea78381c2d"},
		{"a label on the line", "Factory Task", `TASK (from Zach): "Show each agent's model in the tree."`,
			"Factory Task: Show each agent's model in the tree · b7ea78381c2d", "wf-factory-task-show-each-agents-model-in-the-tree-b7ea78381c2d"},

		// a line or sentence that is only a greeting or pleasantry is skipped
		{"greeting line", "Factory Task", "Hi!\nFix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"greeting with a name", "Factory Task", "Hey Claude,\n\nFix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"good morning", "Factory Task", "Good morning —\nFix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"thanks first", "Factory Task", "Thanks!\nFix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"hope line", "Factory Task", "Hope you're well.\nFix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"greeting sentence", "Factory Task", "Hi! Fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"greeting and name in the sentence", "Factory Task", "Hey Claude, can you fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"only a greeting", "Factory Task", "Hi!", "Factory Task: Hi · b7ea78381c2d", "wf-factory-task-hi-b7ea78381c2d"},

		// the title keeps the task's own text, code included; only the branch is a slug
		{"code", "Factory Task", "Fix `if (!ok)` in auth.go",
			"Factory Task: Fix `if (!ok)` in auth.go · b7ea78381c2d", "wf-factory-task-fix-if-ok-in-auth-go-b7ea78381c2d"},
		{"a path", "Factory Task", "go.mod: bump pgx to v5.7",
			"Factory Task: go.mod: bump pgx to v5.7 · b7ea78381c2d", "wf-factory-task-go-mod-bump-pgx-to-v5-7-b7ea78381c2d"},

		// articles stay: "a" can be code or a name
		{"code a", "Factory Task", "Rename `a` to `b`",
			"Factory Task: Rename `a` to `b` · b7ea78381c2d", "wf-factory-task-rename-a-to-b-b7ea78381c2d"},
		{"code span", "Factory Task", "Fix `a + b` calculation",
			"Factory Task: Fix `a + b` calculation · b7ea78381c2d", "wf-factory-task-fix-a-b-calculation-b7ea78381c2d"},
		{"ends on a", "Factory Task", "Rename b to a",
			"Factory Task: Rename b to a · b7ea78381c2d", "wf-factory-task-rename-b-to-a-b7ea78381c2d"},
		{"A record", "Factory Task", "Fix A record for example.com",
			"Factory Task: Fix A record for example.com · b7ea78381c2d", "wf-factory-task-fix-a-record-for-example-com-b7ea78381c2d"},

		// the title keeps code, operators, paths, URLs and quotes; only a mark ending the sentence goes
		{"operators in code", "Factory Task", "Replace `+` with `-`",
			"Factory Task: Replace `+` with `-` · b7ea78381c2d", "wf-factory-task-replace-with-b7ea78381c2d"},
		{"an empty string in code", "Factory Task", "Set the default to `\"\"`",
			"Factory Task: Set the default to `\"\"` · b7ea78381c2d", "wf-factory-task-set-the-default-to-b7ea78381c2d"},
		{"bare operators", "Factory Task", "Change && to ||",
			"Factory Task: Change && to || · b7ea78381c2d", "wf-factory-task-change-to-b7ea78381c2d"},
		{"an operator before the full stop", "Factory Task", "Change && to ||.",
			"Factory Task: Change && to || · b7ea78381c2d", "wf-factory-task-change-to-b7ea78381c2d"},
		{"an empty quoted string", "Factory Task", "Rename \"foo\" to \"\"",
			"Factory Task: Rename \"foo\" to \"\" · b7ea78381c2d", "wf-factory-task-rename-foo-to-b7ea78381c2d"},
		{"a quote with a comma", "Factory Task", "Say \"hello, world\" on the home page",
			"Factory Task: Say \"hello, world\" on the home page · b7ea78381c2d", "wf-factory-task-say-hello-world-on-the-home-page-b7ea78381c2d"},
		{"a flag", "Factory Task", "Add a --force flag to deploy",
			"Factory Task: Add a --force flag to deploy · b7ea78381c2d", "wf-factory-task-add-a-force-flag-to-deploy-b7ea78381c2d"},
		{"a URL", "Factory Task", "Fix the link to https://example.com/docs?page=2.",
			"Factory Task: Fix the link to https://example.com/docs?page=2 · b7ea78381c2d", "wf-factory-task-fix-the-link-to-https-example-com-docs-b7ea78381c2d"},
		{"paths", "Factory Task", "Delete build/cache and dist/ before release.",
			"Factory Task: Delete build/cache and dist/ before release · b7ea78381c2d", "wf-factory-task-delete-build-cache-and-dist-b7ea78381c2d"},
		{"a trailing ellipsis", "Factory Task", "Wait for the deploy...",
			"Factory Task: Wait for the deploy · b7ea78381c2d", "wf-factory-task-wait-for-the-deploy-b7ea78381c2d"},
		{"a lone trailing mark", "Factory Task", "Is it fast enough ?",
			"Factory Task: Is it fast enough ? · b7ea78381c2d", "wf-factory-task-is-it-fast-enough-b7ea78381c2d"},
		{"emoji", "Factory Task", "Ship the release 🚀",
			"Factory Task: Ship the release 🚀 · b7ea78381c2d", "wf-factory-task-ship-the-release-b7ea78381c2d"},
		{"a leading emoji", "Factory Task", "🚀 ship the release",
			"Factory Task: 🚀 ship the release · b7ea78381c2d", "wf-factory-task-ship-the-release-b7ea78381c2d"},
		{"a sentence ending in code", "Factory Task", "Run `go test ./...`",
			"Factory Task: Run `go test ./...` · b7ea78381c2d", "wf-factory-task-run-go-test-b7ea78381c2d"},
		{"a full stop in code", "Factory Task", "Replace `a. b` with c",
			"Factory Task: Replace `a. b` with c · b7ea78381c2d", "wf-factory-task-replace-a-b-with-c-b7ea78381c2d"},
		{"a question in code", "Factory Task", "Is `x > y` true?",
			"Factory Task: Is `x > y` true · b7ea78381c2d", "wf-factory-task-is-x-y-true-b7ea78381c2d"},
		{"an unclosed code span", "Factory Task", "Fix `foo",
			"Factory Task: Fix `foo · b7ea78381c2d", "wf-factory-task-fix-foo-b7ea78381c2d"},
		{"a cut inside a code span", "Factory Task", "Make `alpha beta gamma delta epsilon zeta eta theta` shorter",
			"Factory Task: Make `alpha beta gamma delta epsilon zeta eta…` · b7ea78381c2d", "wf-factory-task-make-alpha-beta-gamma-delta-epsilon-zeta-b7ea78381c2d"},
		{"an abbreviation", "Factory Task", "Fix e.g. the login page",
			"Factory Task: Fix e.g. the login page · b7ea78381c2d", "wf-factory-task-fix-e-g-the-login-page-b7ea78381c2d"},
		{"a greeting and a dash", "Factory Task", "Good morning — fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"please as code", "Factory Task", "`please` is misspelled in the footer",
			"Factory Task: `please` is misspelled in the footer · b7ea78381c2d", "wf-factory-task-please-is-misspelled-in-the-footer-b7ea78381c2d"},
		{"not-equals and an issue", "Factory Task", "Make x != y work: see #12.",
			"Factory Task: Make x != y work: see #12 · b7ea78381c2d", "wf-factory-task-make-x-y-work-see-12-b7ea78381c2d"},
		{"C++", "Factory Task", "Use C++ for the parser.",
			"Factory Task: Use C++ for the parser · b7ea78381c2d", "wf-factory-task-use-c-for-the-parser-b7ea78381c2d"},

		// a lead-in goes only with clear evidence of its end: punctuation, or a request after it
		{"hello world", "Factory Task", "Hello world example is broken",
			"Factory Task: Hello world example is broken · b7ea78381c2d", "wf-factory-task-hello-world-example-is-broken-b7ea78381c2d"},
		{"hey jude", "Factory Task", "Hey Jude cover art is missing",
			"Factory Task: Hey Jude cover art is missing · b7ea78381c2d", "wf-factory-task-hey-jude-cover-art-is-missing-b7ea78381c2d"},
		{"hi-dpi", "Factory Task", "Hi-DPI icons are blurry",
			"Factory Task: Hi-DPI icons are blurry · b7ea78381c2d", "wf-factory-task-hi-dpi-icons-are-blurry-b7ea78381c2d"},
		{"please as a noun", "Factory Task", "Please field is mislabeled",
			"Factory Task: Please field is mislabeled · b7ea78381c2d", "wf-factory-task-please-field-is-mislabeled-b7ea78381c2d"},
		{"could you, no request", "Factory Task", "Could you believe the build is green",
			"Factory Task: Could you believe the build is green · b7ea78381c2d", "wf-factory-task-could-you-believe-the-build-is-green-b7ea78381c2d"},
		{"so, no comma", "Factory Task", "So the build is red",
			"Factory Task: So the build is red · b7ea78381c2d", "wf-factory-task-so-the-build-is-red-b7ea78381c2d"},
		{"ok so, no comma", "Factory Task", "OK so the deploy is red",
			"Factory Task: OK so the deploy is red · b7ea78381c2d", "wf-factory-task-ok-so-the-deploy-is-red-b7ea78381c2d"},
		{"hope you will be able", "Factory Task", "I hope you will be able to fix the login page",
			"Factory Task: I hope you will be able to fix · b7ea78381c2d", "wf-factory-task-i-hope-you-will-be-able-to-fix-b7ea78381c2d"},
		{"thanks for, no comma", "Factory Task", "Thanks for the fix but the login still crashes",
			"Factory Task: Thanks for the fix but the login still · b7ea78381c2d", "wf-factory-task-thanks-for-the-fix-but-the-login-still-b7ea78381c2d"},
		{"kindly as a name, and an abbreviation", "Factory Task", "Kindly Inc. invoices are late",
			"Factory Task: Kindly Inc. invoices are late · b7ea78381c2d", "wf-factory-task-kindly-inc-invoices-are-late-b7ea78381c2d"},
		{"hello, then a request", "Factory Task", "Hello, fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"hey name, then a request", "Factory Task", "Hey Claude, fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"hey, then a polite request", "Factory Task", "Hey can you fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"ok, then a request", "Factory Task", "OK, so please fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"please do not", "Factory Task", "Please do not deploy on Friday",
			"Factory Task: Do not deploy on Friday · b7ea78381c2d", "wf-factory-task-do-not-deploy-on-friday-b7ea78381c2d"},
		{"thanks, then a polite request", "Factory Task", "Thanks can you fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},

		// limits cut at whole words, and a name cut short never ends on an article or connecting word
		{"a long sentence", "Factory Task",
			"Add a button to the settings page that exports every account's hosts as CSV for auditing",
			"Factory Task: Add a button to the settings page · b7ea78381c2d",
			"wf-factory-task-add-a-button-to-the-settings-page-b7ea78381c2d"},
		{"a long workflow name", "The Very Long Workflow Name That Goes On And On", "Fix the login page",
			"The Very Long Workflow Name That Goes On And On: Fix the login page · b7ea78381c2d",
			"wf-the-very-long-workflow-fix-the-login-page-b7ea78381c2d"},
		{"one very long word", "Factory Task", "Supercalifragilisticexpialidociousandthensomemoreletters now",
			"Factory Task: Supercalifragilisticexpialidociousandthensomemoreletters now · b7ea78381c2d",
			"wf-factory-task-supercalifragilisticexpialidociousandthe-b7ea78381c2d"},

		// a word or a spaceless script longer than a title is cut, marked with …; in the branch, to 40
		{"a 150-character word", "Factory Task", strings.Repeat("x", 150),
			"Factory Task: " + strings.Repeat("x", 59) + "… · b7ea78381c2d", "wf-factory-task-" + strings.Repeat("x", 40) + "-b7ea78381c2d"},
		{"a long sentence without spaces", "Factory Task", strings.Repeat("修复登录页面", 20),
			"Factory Task: " + string([]rune(strings.Repeat("修复登录页面", 20))[:59]) + "… · b7ea78381c2d", "wf-factory-task-b7ea78381c2d"},

		// a social lead-in goes and the task after it stays; a sentence that is only social is skipped
		{"thanks then a task", "Factory Task", "Thanks, please fix the login page. Do not deploy.",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"thanks for, but", "Factory Task", "Thanks for the fix, but login still crashes. Please investigate.",
			"Factory Task: Login still crashes · b7ea78381c2d", "wf-factory-task-login-still-crashes-b7ea78381c2d"},
		{"thank you and a dash", "Factory Task", "Thank you — fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"appreciate it", "Factory Task", "Appreciate it, can you fix the login page?",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"sorry to bother", "Factory Task", "Sorry to bother you, but the deploy is failing",
			"Factory Task: The deploy is failing · b7ea78381c2d", "wf-factory-task-the-deploy-is-failing-b7ea78381c2d"},
		{"hope this helps, then a task", "Factory Task", "Hope this helps — the login still fails.",
			"Factory Task: The login still fails · b7ea78381c2d", "wf-factory-task-the-login-still-fails-b7ea78381c2d"},
		{"thank you so much, alone", "Factory Task", "Thank you so much.\nFix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"hope this helps, alone", "Factory Task", "Hope this helps! Fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"thanks in advance", "Factory Task", "Thanks in advance for fixing the login page",
			"Factory Task: Fixing the login page · b7ea78381c2d", "wf-factory-task-fixing-the-login-page-b7ea78381c2d"},
		{"a thanks page", "Factory Task", "Thanks page shows wrong totals",
			"Factory Task: Thanks page shows wrong totals · b7ea78381c2d", "wf-factory-task-thanks-page-shows-wrong-totals-b7ea78381c2d"},
		{"hope you can, then more", "Factory Task", "I hope you can fix the login page. Do not deploy.",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"hope you can add", "Factory Task", "Hope you can add CSV export. Keep the UI simple.",
			"Factory Task: Add CSV export · b7ea78381c2d", "wf-factory-task-add-csv-export-b7ea78381c2d"},
		{"hope you could", "Factory Task", "Hope you could look at the flaky test",
			"Factory Task: Look at the flaky test · b7ea78381c2d", "wf-factory-task-look-at-the-flaky-test-b7ea78381c2d"},
		{"hoping you can", "Factory Task", "Hoping you can rerun CI today",
			"Factory Task: Rerun CI today · b7ea78381c2d", "wf-factory-task-rerun-ci-today-b7ea78381c2d"},
		{"hope we can: unrecognised, kept", "Factory Task", "I hope we can ship this today",
			"Factory Task: I hope we can ship this today · b7ea78381c2d", "wf-factory-task-i-hope-we-can-ship-this-today-b7ea78381c2d"},
		{"hope the deploy: unrecognised, kept", "Factory Task", "Hope the deploy works now. Check it.",
			"Factory Task: Hope the deploy works now · b7ea78381c2d", "wf-factory-task-hope-the-deploy-works-now-b7ea78381c2d"},
		{"hope you're well, then a task", "Factory Task", "I hope you're well, can you fix the login page?",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"an unlisted thanks clause is kept", "Factory Task", "Thanks for nothing, the login still crashes",
			"Factory Task: Thanks for nothing, the login still crashes · b7ea78381c2d", "wf-factory-task-thanks-for-nothing-the-login-still-b7ea78381c2d"},
		{"social with emoji", "Factory Task", "Thank you so much!!! 🙏\nFix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},
		{"please then", "Factory Task", "Please now fix the login page",
			"Factory Task: Now fix the login page · b7ea78381c2d", "wf-factory-task-now-fix-the-login-page-b7ea78381c2d"},
		{"two lead-ins", "Factory Task", "Thanks, hope you're well, and please fix the login page",
			"Factory Task: Fix the login page · b7ea78381c2d", "wf-factory-task-fix-the-login-page-b7ea78381c2d"},

		// a long first word is cut at its hyphens, or truncated, never dropped from the branch
		{"a long hyphenated workflow name", "dependency-vulnerability-review", "Update pgx",
			"dependency-vulnerability-review: Update pgx · b7ea78381c2d", "wf-dependency-vulnerability-update-pgx-b7ea78381c2d"},
		{"a one-word workflow name too long", "Supercalifragilisticexpialidocious", "Update pgx",
			"Supercalifragilisticexpialidocious: Update pgx · b7ea78381c2d", "wf-supercalifragilisticexpi-update-pgx-b7ea78381c2d"},
		{"a long URL first", "Factory Task", "https://example.com/a/very/long/path/to/some/resource is broken",
			"Factory Task: https://example.com/a/very/long/path/to/some/resource is · b7ea78381c2d",
			"wf-factory-task-https-example-com-a-very-long-path-b7ea78381c2d"},
		{"a long identifier first", "Factory Task", "TestHostSendsItsWorkspacesDiffsWhenTheyChangeQuickly flakes",
			"Factory Task: TestHostSendsItsWorkspacesDiffsWhenTheyChangeQuickly flakes · b7ea78381c2d",
			"wf-factory-task-testhostsendsitsworkspacesdiffswhentheyc-b7ea78381c2d"},

		// the title keeps any script; the branch keeps ASCII (Paseo accepts nothing else), folding accents
		{"accents", "Tâches", "Café: añadir los platos del día",
			"Tâches: Café: añadir los platos del día · b7ea78381c2d", "wf-taches-cafe-anadir-los-platos-del-dia-b7ea78381c2d"},
		{"no ASCII task", "Factory Task", "修复登录页面",
			"Factory Task: 修复登录页面 · b7ea78381c2d", "wf-factory-task-b7ea78381c2d"},
		{"no ASCII at all", "任务", "修复登录页面", "任务: 修复登录页面 · b7ea78381c2d", "wf-b7ea78381c2d"},
		{"no words: the first line stands in", "Factory Task", "🙏",
			"Factory Task: 🙏 · b7ea78381c2d", "wf-factory-task-b7ea78381c2d"},
		{"only a heading and an emoji", "Factory Task", "Task:\n🙏", "Factory Task: Task: · b7ea78381c2d", "wf-factory-task-b7ea78381c2d"},
		{"no workflow name", "", "Fix the login page", "Fix the login page · b7ea78381c2d", "wf-fix-the-login-page-b7ea78381c2d"},
	} {
		title, br := runNames(id, c.workflow, taskName(c.input))
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
	ta, ba := runNames("aaaaaaaa-1111-4000-8000-000000000000", "Factory Task", taskName("Fix the login page"))
	_, bb := runNames("aaaaaaaa-2222-4000-8000-000000000000", "Factory Task", taskName("Fix the login page"))
	if ba == bb || !strings.HasSuffix(ba, "-aaaaaaaa1111") {
		t.Errorf("branches %q and %q", ba, bb)
	}
	tc, _ := runNames("aaaaaaaa-2222-4000-8000-000000000000", "Factory Task", taskName("Fix the login page"))
	if ta == tc || !strings.HasSuffix(ta, " · aaaaaaaa1111") {
		t.Errorf("titles %q and %q", ta, tc)
	}
}

func TestAgentTitlesNameTheRoleWorkflowAndTask(t *testing.T) {
	if got := agentTitle("Orchestrator", "Factory Task", "Fix the login page"); got != "Orchestrator · Factory Task: Fix the login page" {
		t.Errorf("agentTitle = %q", got)
	}
	if got := agentTitle("Orchestrator", "", "Fix the login page"); got != "Orchestrator · Fix the login page" {
		t.Errorf("agentTitle without a workflow name = %q", got)
	}
}
