package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func TestSavedEmailWorkflowGetsFollowUpReplyRules(t *testing.T) {
	d := Starting
	d.Trigger = TriggerEmail
	d.Orchestrator.Instructions = "Saved instructions without the new rules."
	p, err := OrchestratorPrompt("Saved email task", d, Vars{Run: "follow-up", Input: "Did you do it?"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Correction:", "number, recommendation or yes/no", "Done", "Changed", "Nothing was changed", "first words must be yes/no", "earlier sent answers do not count"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestOrchestratorSummarizesTheTaskBeforeStartingWork(t *testing.T) {
	p := mustOrchestratorPrompt(t, "Saved workflow", Starting, Vars{Run: "run-1", Input: "a long task"})
	for _, want := range []string{"intelligence workflow summarize --run run-1 --summary", "3–8 words", "whole task", "If saving the summary fails, continue"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing summary instruction %q", want)
		}
	}
}

func TestSavedOrchestratorPreservesNamesBeforeSummarizing(t *testing.T) {
	d := clone(Starting)
	d.Orchestrator.Instructions = "Custom saved workflow instructions"
	p := mustOrchestratorPrompt(t, "Saved", d, Vars{Run: "run-1", Input: "Send the transcript to Maxwell"})
	for _, want := range []string{
		"In summaries, run titles and email subjects, keep people's names as Zach said or wrote them",
		"never add a surname or identity unless this run verified it with evidence",
		"Cite the source used to verify an identity in the reply",
		"Generated titles and summaries are not identity evidence",
		"Leave unresolved names as given",
		"If you choose among ambiguous names, say who and why in one reply line, citing the source",
		"Pass this names rule to every worker",
	} {
		at := strings.Index(p, want)
		if at < 0 || at > strings.Index(p, "intelligence workflow summarize --run") {
			t.Errorf("names rule must precede summarize command: missing %q", want)
		}
	}
}

func TestStartingWorkflowRecommendsSubscribedPlans(t *testing.T) {
	if strings.Contains(OrchestratorInstructions, "antigravity/") || strings.Contains(OrchestratorInstructions, "muse/") {
		t.Error("starting guidance recommends an unsubscribed provider")
	}
	p := mustOrchestratorPrompt(t, "task", Starting, Vars{Run: "r"})
	for _, want := range []string{"claude/claude-opus-5-5", "codex"} {
		if !strings.Contains(p, want) {
			t.Errorf("frontier guidance missing %q", want)
		}
	}
}

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
		"antigravity orch thinking": {func(d *Definition) {
			d.Orchestrator.Provider = "antigravity"
			d.Orchestrator.Model = "gemini-4-neon-medium"
			d.Orchestrator.Thinking = "medium"
		}, "model ID"},
		"tier on claude": {func(d *Definition) {
			d.Orchestrator.ServiceTier = "ultrafast"
		}, "service tier"},
		"unknown tier": {func(d *Definition) {
			d.Orchestrator.Provider = "codex"
			d.Orchestrator.ServiceTier = "ludicrous"
		}, "service tier"},
		"finder tier on claude": {func(d *Definition) {
			d.Project.ServiceTier = "priority"
		}, "service tier"},
		"antigravity proj thinking": {func(d *Definition) {
			d.Project.Provider = "antigravity"
			d.Project.Model = "gemini-4-neon-medium"
			d.Project.Thinking = "high"
		}, "model ID"},
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
	fixed.Project = Project{Mode: "fixed", Directory: "/home/someone/dev/app", Provider: "antigravity", Thinking: "medium"}
	fixed.Orchestrator.Provider = "antigravity"
	fixed.Orchestrator.Model = "gemini-4-neon-medium"
	fixed.Orchestrator.Thinking = ""
	b, _ := json.Marshal(fixed)
	if _, err := Parse(b); err != nil {
		t.Errorf("a fixed project needs no agent and antigravity without thinking is valid: %v", err)
	}
}

func TestCodexAgentsTakeAServiceTier(t *testing.T) {
	for _, tier := range []string{"", "priority", "ultrafast"} {
		d := onHost(clone(Starting))
		d.Orchestrator = Orchestrator{Provider: "codex", Model: "gpt-6.2-sol", Thinking: "high", ServiceTier: tier, Instructions: "x"}
		d.Project.Provider, d.Project.ServiceTier = "codex", tier
		b, _ := json.Marshal(d)
		got, err := Parse(b)
		if err != nil || got.Orchestrator.ServiceTier != tier || got.Project.ServiceTier != tier {
			t.Errorf("tier %q: %+v, %v", tier, got.Orchestrator, err)
		}
		if tier == "" && strings.Contains(string(b), "serviceTier") {
			t.Errorf("no tier should leave the saved definition unchanged: %s", b)
		}
	}
}

func TestOrchestratorPinsWorkersToItsWorkspace(t *testing.T) {
	p := mustOrchestratorPrompt(t, "Tasks", Starting, Vars{Run: "run-1", Repo: "/w/files", Branch: "fix-files"})
	for _, want := range []string{"--workspace", "paseo inspect", "working directory", "Stop a misplaced worker"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in prompt", want)
		}
	}
}

func TestSavedWorkflowStepsRequirePublicFileDelivery(t *testing.T) {
	d := clone(Starting)
	d.Orchestrator.Instructions = "Custom saved instructions"
	for i := range d.Steps {
		for _, prompt := range []string{mustOrchestratorPrompt(t, "Saved", d, Vars{}), mustStepMessage(t, d, i, Vars{})} {
			for _, want := range []string{"files.zachlatta.com", "public, unguessable", "no authentication", "file-share", "anonymous GET", "text/plain; charset=utf-8", "Pass this file-delivery rule to every worker"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("step %d misses file-delivery instruction %q", i, want)
				}
			}
		}
	}
}

// A run whose result is a report, answer or plan for Zach, rather than code or a host change, must
// end with that result in his inbox, whatever the repository and whether or not anything lands.
// Saved workflows get the rule too: runs of the saved Factory Task in other repositories than the
// one its landing step named finished with the result only on the run page.
func TestSavedWorkflowStepsEmailDeliverablesEvenWhenNothingLands(t *testing.T) {
	d := clone(Starting)
	d.Orchestrator.Instructions = "Custom saved instructions"
	d.Steps[len(d.Steps)-1].Instructions = "Land on main the way AGENTS.md says."
	for i := range d.Steps {
		for _, prompt := range []string{mustOrchestratorPrompt(t, "Saved", d, Vars{}), mustStepMessage(t, d, i, Vars{})} {
			for _, want := range []string{
				"report, investigation, answer, proposal, plan, deck or analysis",
				"in any repository", "even when nothing lands",
				"zrl-bot", "zrl-bot-email skill", "full substance in the body",
				"Earlier steps draft", "only the last step sends", "when the run ends early (fail or skip to the end)",
				"search zrl-bot's sent mail from the run's request date", "by subject", "by the report's URL",
				"in the request thread", "Inspect every match", "a recent-days window",
				"once", "Pass this deliverable-email rule to every worker",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("step %d misses deliverable-email instruction %q", i, want)
				}
			}
		}
	}
}

func TestSignoffPermitsFileDeliveryAndProtectsPrivateData(t *testing.T) {
	for _, prompt := range []string{mustOrchestratorPrompt(t, "Tasks", Starting, Vars{}), mustStepMessage(t, Starting, 4, Vars{})} {
		for _, want := range []string{
			"allowed in any step, including sign-off", "not deferred to landing",
			"unless a step explicitly forbids uploads", "donor, contact, financial or student data",
			"unless Zach explicitly asks to publish that data", "restricted to Zach's verified email address from the file-share skill",
			"link them as text", "anonymous GET returns 401",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt misses %q", want)
			}
		}
		if strings.Contains(prompt, "Do not upload before") {
			t.Error("generic external-write ban still blocks sign-off delivery")
		}
	}
}

// Aggregate metrics are not "student or financial data" on a literal reading, so the rule names
// internal metrics too: they stay restricted, aggregate or not.
func TestFileDeliveryKeepsInternalMetricsRestricted(t *testing.T) {
	for _, want := range []string{"internal metrics", "counts, rates or spend about people, programs or money",
		"aggregate"} {
		if !strings.Contains(FileDeliveryRule, want) {
			t.Errorf("file-delivery rule misses %q", want)
		}
	}
}

func TestTheProjectFinderIsToldTheRepositoriesAndHowToAnswer(t *testing.T) {
	p := mustProjectPrompt(t, "run-1", Starting, "Show each agent's model", []string{"/srv/dev/app", "/srv/dev/site"})
	for _, want := range []string{Starting.Project.Instructions, "Show each agent's model", "- /srv/dev/app\n- /srv/dev/site",
		"intelligence workflow project --run run-1 <directory>", "intelligence workflow project --run run-1 none"} {
		if !strings.Contains(p, want) {
			t.Errorf("project prompt misses %q:\n%s", want, p)
		}
	}
}

func TestProjectFinderRoutesDeliverablesByOutputRatherThanDataSource(t *testing.T) {
	for _, instructions := range []string{Starting.Project.Instructions, "Pick the repository this task belongs in."} {
		d := Starting
		d.Project.Instructions = instructions // Existing saved definitions get the policy too.
		p := mustProjectPrompt(t, "run-1", d,
			"Give me a report (deck) on program growth using warehouse data.",
			[]string{"/srv/dev/data-warehouse", "/srv/dev/desk"})
		for _, want := range []string{
			"Choose by the requested work product",
			"reports, analyses, decks, emails, research",
			"choose desk when it is among the candidates",
			"Reading or querying a warehouse does not make its code repository the task's workspace",
			"changes to a repository's code, tests, configuration or maintained documentation",
			"Honor an explicit workspace choice",
			"If desk is absent",
			"Do not invent a checkout path",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("project prompt lacks routing guidance %q (instructions %q)", want, instructions)
			}
		}
	}
}

func TestTheOrchestratorGetsTheRunTheStepsAndTheFirstStep(t *testing.T) {
	vars := Vars{Run: "run-1", Input: "Show each agent's model", Repo: "/srv/dev/app", Branch: "wf-run1"}
	p := mustOrchestratorPrompt(t, "Task from Intelligence", Starting, vars)
	for _, want := range []string{Starting.Orchestrator.Instructions, "run run-1 of the workflow “Task from Intelligence”",
		"Show each agent's model", "1. Do the work (step, id build)", "5. Zach's sign-off (signoff, id signoff)",
		"intelligence workflow next --run run-1", "intelligence workflow back --run run-1 <step id>",
		"intelligence workflow ask --run run-1 --note", "Step 1 of 7: Do the work"} {
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
	got := mustStepMessage(t, d, 0, Vars{Run: "r", Input: "it", Repo: "/a", Branch: "b"})
	if !strings.Contains(got, "Step 1 of 7: Do the work") || !strings.Contains(got, "Do it in /a on b for run r; leave {{unknown}}.") {
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
	p := mustOrchestratorPrompt(t, "Task", d, Vars{Run: "run-1", Input: "x"})
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
	p := mustOrchestratorPrompt(t, "Saved workflow", d, Vars{Run: "run-1"})
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
	if p := mustOrchestratorPrompt(t, "Saved workflow", d, Vars{Run: "run-1"}); !strings.Contains(p, want) {
		t.Errorf("saved workflow does not assign approved external changes to landing")
	}
	if p := mustStepMessage(t, Starting, Starting.Index("land"), Vars{Run: "run-1"}); !strings.Contains(p, want) {
		t.Errorf("landing step does not own approved external changes")
	}
}

// Zach does not want pull requests unless he asks for one. Orchestrators copied them from a
// repository's history ("merge it as this repo does: past PRs #119 and #120"), so every run,
// saved definitions included, is told to land straight on main.
func TestWorkflowsLandWithoutPullRequests(t *testing.T) {
	saved := clone(Starting)
	saved.Orchestrator.Instructions = "Coordinate this task."
	for i := range saved.Steps {
		saved.Steps[i].Instructions = "Do the authorized step."
	}
	for name, d := range map[string]Definition{"starting": Starting, "saved": saved} {
		t.Run(name, func(t *testing.T) {
			prompts := []string{mustOrchestratorPrompt(t, "task", d, Vars{Run: "r"})}
			for i := range d.Steps {
				prompts = append(prompts, mustStepMessage(t, d, i, Vars{Run: "r"}))
			}
			for i, p := range prompts {
				if n := strings.Count(p, NoPullRequestRule); n != 1 {
					t.Errorf("prompt %d has the no-pull-request rule %d times, want once", i, n)
				}
			}
		})
	}
	for _, want := range []string{"Never open a pull request", "gh pr create", "unless Zach's request explicitly asks for one",
		"straight to main", "AGENTS.md", "history", "ask Zach", "Pass this rule to every worker"} {
		if !strings.Contains(NoPullRequestRule, want) {
			t.Errorf("no-pull-request rule misses %q", want)
		}
	}
	land := Starting.Steps[Starting.Index("land")].Instructions
	if !strings.Contains(land, "without a pull request") {
		t.Errorf("starting landing step does not say to land without a pull request: %q", land)
	}
}

func TestWorkflowCredentialHandoffRule(t *testing.T) {
	for _, p := range []string{mustOrchestratorPrompt(t, "Saved", Starting, Vars{}), mustStepMessage(t, Starting, 0, Vars{})} {
		for _, want := range []string{"Never paste credentials", "environment variable", "0600"} {
			if !strings.Contains(p, want) {
				t.Errorf("missing credential handoff rule %q", want)
			}
		}
	}
}

func TestWorkflowPromptsAvoidPermissionTraps(t *testing.T) {
	for _, p := range []string{mustOrchestratorPrompt(t, "task", Starting, Vars{Run: "r"}), mustStepMessage(t, Starting, 0, Vars{})} {
		if n := strings.Count(p, UnattendedShellRule); n != 1 {
			t.Errorf("unattended guidance occurs %d times, want once", n)
		}
		for _, want := range []string{"Write/Edit", "heredoc"} {
			if !strings.Contains(p, want) {
				t.Errorf("prompt misses %q", want)
			}
		}
	}
}

func TestWorkflowPromptsDenyRemovalAndPreserveScratch(t *testing.T) {
	saved := clone(Starting)
	saved.Orchestrator.Instructions = "Coordinate this task."
	for i := range saved.Steps {
		saved.Steps[i].Instructions = "Do the authorized step."
	}
	for name, d := range map[string]Definition{"starting": Starting, "saved": saved} {
		t.Run(name, func(t *testing.T) {
			prompts := []string{mustOrchestratorPrompt(t, "task", d, Vars{Run: "r"})}
			for i := range d.Steps {
				prompts = append(prompts, mustStepMessage(t, d, i, Vars{Run: "r"}))
			}
			for i, p := range prompts {
				// The wording is pinned once, in hostclient; prompts reuse the denial's own text.
				for _, want := range []string{hostclient.RemovalRule, "Do not retry the same blocked command"} {
					if !strings.Contains(p, want) {
						t.Errorf("prompt %d misses %q", i, want)
					}
				}
				for _, bad := range []string{"literal absolute paths", "delete scratch", "remove scratch"} {
					if strings.Contains(p, bad) {
						t.Errorf("prompt %d contains %q", i, bad)
					}
				}
			}
		})
	}
}

func TestUnattendedModeGuidanceIsNotDuplicated(t *testing.T) {
	p := mustOrchestratorPrompt(t, "task", Starting, Vars{Run: "r"})
	for _, provider := range Providers() {
		want := "--mode " + agentMode(provider) + " for " + provider
		if n := strings.Count(p, want); n != 1 {
			t.Errorf("mode guidance %q appears %d times", want, n)
		}
	}
	if strings.Contains(UnattendedShellRule, "--mode ") {
		t.Fatal("shell guidance duplicates the shared mode guidance")
	}
}

// Reports and data pulls are results, not changes to a repository. A finder that picks the
// repository the data comes from publishes them when that repository is public.
func TestTheProjectFinderKeepsResultsOutOfPublicRepositories(t *testing.T) {
	d := clone(Starting)
	d.Project.Instructions = "Pick the repository this task belongs in."
	p := mustProjectPrompt(t, "run-1", d, "Give me a report on growth", []string{"/srv/dev/warehouse", "/srv/dev/notes"})
	if n := strings.Count(p, ProjectRule); n != 1 {
		t.Fatalf("the finder's standing rule occurs %d times, want once:\n%s", n, p)
	}
	for _, want := range []string{"report", "metrics", "private repository", "even when its data comes from",
		"gh repo view", "Never pick a public repository for a result"} {
		if !strings.Contains(ProjectRule, want) {
			t.Errorf("finder rule misses %q", want)
		}
	}
}

// Every run, saved definitions included, is told what a public repository must never get.
func TestSavedWorkflowsKeepResultsOutOfPublicRepositories(t *testing.T) {
	d := clone(Starting)
	d.Orchestrator.Instructions = "Coordinate this task."
	d.Steps = d.Steps[:1]
	p := mustOrchestratorPrompt(t, "Saved workflow", d, Vars{Run: "run-1", Repo: "/srv/dev/warehouse"})
	if n := strings.Count(p, PublicRepoRule); n != 1 {
		t.Fatalf("the public repository rule occurs %d times, want once", n)
	}
	for _, want := range []string{"work/", "metrics", "CSV", "student", "Aggregate-only", ".factory-scratch/",
		"git log -m --stat origin/main..HEAD", "wrong repository"} {
		if !strings.Contains(PublicRepoRule, want) {
			t.Errorf("public repository rule misses %q", want)
		}
	}
}

func TestTheStartingWorkflowChecksPublicRepositoriesBeforeLanding(t *testing.T) {
	i := Starting.Index("public")
	if i < 0 || Starting.Steps[i].Kind != KindGate || i != Starting.Index("land")-1 {
		t.Fatalf("want a public gate just before landing, got index %d", i)
	}
	s := Starting.Steps[i].Instructions
	for _, want := range []string{"gh repo view", "skip this step", "fresh sub-agent", "origin/main", "work/",
		"metrics", "Aggregate-only", "back to Do the work"} {
		if !strings.Contains(s, want) {
			t.Errorf("public gate misses %q", want)
		}
	}
}

// Audits and walkthroughs can hold private operational details and no counts at all: a public
// link to one publishes those details just the same.
func TestFileDeliveryKeepsInternalReportsWithoutMetricsRestricted(t *testing.T) {
	for _, want := range []string{"every internal report", "audits", "reviews", "walkthroughs",
		"incidents", "security weaknesses", "credential names", "even redacted", "even with no metrics"} {
		if !strings.Contains(FileDeliveryRule, want) {
			t.Errorf("file-delivery rule misses %q", want)
		}
	}
	// Public delivery stays the default for everything else.
	if !strings.Contains(FileDeliveryRule, "public, unguessable URLs with no authentication") {
		t.Error("file-delivery rule lost its public default")
	}
}

// A report committed and deleted again before the push is still in the pushed history, so the
// public repository checks read every outgoing commit, not the final diff.
func TestPublicRepositoryChecksReadEveryOutgoingCommit(t *testing.T) {
	gate := Starting.Steps[Starting.Index("public")].Instructions
	for name, text := range map[string]string{"contract": PublicRepoRule, "gate": gate} {
		for _, want := range []string{"git log -m --stat origin/main..HEAD", "merge commits", "every outgoing commit",
			"deleted in a later commit", "rewrite"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s misses %q", name, want)
			}
		}
		if strings.Contains(text, "git diff --stat origin/main...HEAD") {
			t.Errorf("%s still reviews only the final diff", name)
		}
	}
	if !strings.Contains(gate, "reports outside work/") {
		t.Error("gate does not look for reports outside work/")
	}
}

func TestOrchestratorLifecycleForEveryDefinition(t *testing.T) {
	for _, d := range []Definition{Starting, {Orchestrator: Orchestrator{Instructions: "Custom"}, Steps: []Step{{ID: "one", Title: "One"}}}} {
		p := mustOrchestratorPrompt(t, "Saved", d, Vars{})
		for _, want := range []string{"after next, skip or back", "same turn", "foreground", "only after a successful ask", "waiting on model quota", "do NOT ask Zach", "--exclude"} {
			if !strings.Contains(p, want) {
				t.Errorf("missing lifecycle/quota rule %q", want)
			}
		}
	}
}

func TestParseRejectsRemovedStepModelFields(t *testing.T) {
	b, _ := json.Marshal(onHost(clone(Starting)))
	for _, field := range []string{`"model":"{{frontier_model}}"`, `"model":"codex/sol"`, `"thinking":"high"`} {
		raw := strings.Replace(string(b), `"steps":[{`, `"steps":[{`+field+",", 1)
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("removed step field %s: %v", field, err)
		}
	}
}

func mustProjectPrompt(t *testing.T, run string, d Definition, input string, repos []string) string {
	t.Helper()
	p, err := ProjectPrompt(run, d, input, repos)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func mustOrchestratorPrompt(t *testing.T, name string, d Definition, v Vars) string {
	t.Helper()
	p, err := OrchestratorPrompt(name, d, v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func mustStepMessage(t *testing.T, d Definition, i int, v Vars) string {
	t.Helper()
	p, err := StepMessage(d, i, v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
