// Package workflow runs workflows: a trigger's input, a project found for it, and an orchestrator
// agent in that project's worktree that is handed the workflow's steps one at a time.
//
// The orchestrator never does the work itself: it starts sub-agents for it and decides when each
// step is done, moving the run with `intelligence workflow` (next, back, skip, ask, done, fail),
// which reaches the server through the host's key. The server only records the run and hands
// the orchestrator the next step's instructions.
package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/paseo"
)

// Definition is one version of a workflow: the host it runs on, what starts it, how its project
// is found, its orchestrator, and its steps, in order.
type Definition struct {
	Host         string       `json:"host"`            // the account's host every run happens on
	Trigger      string       `json:"trigger"`         // task (submitted on the Workflows page), manual or email
	Email        *EmailFilter `json:"email,omitempty"` // email: which email starts a run
	Project      Project      `json:"project"`
	Orchestrator Orchestrator `json:"orchestrator"`
	Steps        []Step       `json:"steps"`
}

// Project is how a run's project is found, before the orchestrator starts in its worktree: an
// agent picks one of the host's repositories (match), or it is always Directory (fixed).
type Project struct {
	Mode         string `json:"mode"`
	Directory    string `json:"directory,omitempty"` // fixed: the repository's main checkout on the host
	Provider     string `json:"provider,omitempty"`  // match: the finder agent
	Model        string `json:"model,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// Orchestrator is the agent that runs the steps, and its standing instructions.
type Orchestrator struct {
	Provider     string `json:"provider"`
	Model        string `json:"model,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	Instructions string `json:"instructions"`
}

// Step is instructions to the orchestrator. Its kind changes only how it reads: a step does work,
// a gate decides whether to move on or send work back, and a sign-off may ask Zach.
type Step struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Instructions string `json:"instructions"`
}

// Triggers, project modes and step kinds.
const (
	TriggerTask   = "task"
	TriggerManual = "manual"
	TriggerEmail  = "email" // an email to the account's connected Gmail matches Email
	ProjectMatch  = "match"
	ProjectFixed  = "fixed"
	KindStep      = "step"
	KindGate      = "gate"
	KindSignoff   = "signoff"
)

var stepID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Parse decodes a definition strictly and validates it.
func Parse(b []byte) (Definition, error) {
	var d Definition
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("bad workflow: %w", err)
	}
	return d, d.Validate()
}

// Validate says what in d cannot run.
func (d Definition) Validate() error {
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	switch {
	case d.Host == "":
		return errors.New("a workflow needs a host to run on")
	case !hostclient.ValidName(d.Host):
		return fmt.Errorf("host %q is not a host's name", d.Host)
	case d.Trigger != TriggerTask && d.Trigger != TriggerManual && d.Trigger != TriggerEmail:
		return fmt.Errorf("trigger %q: want %s, %s or %s", d.Trigger, TriggerTask, TriggerManual, TriggerEmail)
	case d.Trigger == TriggerEmail && d.Email == nil:
		return errors.New("an email trigger needs an email filter: which senders, address or words start a run")
	case d.Trigger != TriggerEmail && d.Email != nil:
		return fmt.Errorf("only an email trigger has an email filter, not %s", d.Trigger)
	case d.Project.Mode == ProjectFixed && (d.Project.Directory == "" || !path.IsAbs(d.Project.Directory)):
		return errors.New("a fixed project needs the absolute directory of its repository on the host")
	case d.Project.Mode == ProjectMatch && blank(d.Project.Provider):
		return errors.New("the project finder needs a provider")
	case d.Project.Mode == ProjectMatch && blank(d.Project.Instructions):
		return errors.New("the project finder needs instructions")
	case d.Project.Mode != ProjectMatch && d.Project.Mode != ProjectFixed:
		return fmt.Errorf("project mode %q: want %s or %s", d.Project.Mode, ProjectMatch, ProjectFixed)
	case blank(d.Orchestrator.Provider):
		return errors.New("the orchestrator needs a provider")
	case blank(d.Orchestrator.Instructions):
		return errors.New("the orchestrator needs instructions")
	case len(d.Steps) == 0:
		return errors.New("a workflow needs at least one step")
	}
	if d.Email != nil {
		if err := d.Email.Normalize(); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i, s := range d.Steps {
		n := i + 1
		switch {
		case !stepID.MatchString(s.ID):
			return fmt.Errorf("step %d: id %q: lowercase letters, digits and -, up to 32", n, s.ID)
		case seen[s.ID]:
			return fmt.Errorf("step %d: id %q is used twice", n, s.ID)
		case s.Kind != KindStep && s.Kind != KindGate && s.Kind != KindSignoff:
			return fmt.Errorf("step %d: kind %q: want step, gate or signoff", n, s.Kind)
		case blank(s.Title):
			return fmt.Errorf("step %d needs a title", n)
		case blank(s.Instructions):
			return fmt.Errorf("step %d (%s) needs instructions", n, s.Title)
		}
		seen[s.ID] = true
	}
	return nil
}

// Index is the position of the step with id, or -1.
func (d Definition) Index(id string) int {
	for i, s := range d.Steps {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// Vars fill a step's instructions: {{run}}, {{input}}, {{repo}} and {{branch}}.
type Vars struct {
	Run, Input, Repo, Branch string
}

func (v Vars) fill(s string) string {
	return strings.NewReplacer("{{run}}", v.Run, "{{input}}", v.Input, "{{repo}}", v.Repo, "{{branch}}", v.Branch).Replace(s)
}

// ProjectPrompt is what the project finder is told: its instructions, the input, the host's
// repositories, and how to answer.
func ProjectPrompt(run string, d Definition, input string, repos []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nThe task:\n\n%s\n\n", strings.TrimSpace(d.Project.Instructions), strings.TrimSpace(input))
	if len(repos) == 0 {
		b.WriteString("This host has no repositories in Paseo yet.\n\n")
	} else {
		fmt.Fprintf(&b, "The repositories on this host (their main checkouts):\n\n- %s\n\n", strings.Join(repos, "\n- "))
	}
	fmt.Fprintf(&b, "Change nothing. When you have decided, run one of these, then stop:\n\n"+
		"  intelligence workflow project --run %[1]s <directory>\n"+
		"  intelligence workflow project --run %[1]s none\n\n"+
		"none means the task belongs in no repository: the orchestrator then works in the home directory.", run)
	return b.String()
}

// Providers are the providers the workflow editor offers: those with a mode that never asks.
func Providers() []string { return paseo.UnattendedProviders() }

// agentMode is the provider's unattended mode, or its default for unknown providers.
func agentMode(provider string) string { return paseo.UnattendedMode(provider) }

// subAgentModes tells the orchestrator which mode to start each provider's sub-agents in. It is
// in the prompt, not only the instructions, so definitions saved before it get it too: `paseo
// run` starts an agent in its provider's default mode, which may ask, unless given --mode.
func subAgentModes() string {
	providers := Providers()
	modes := make([]string, len(providers))
	for i, provider := range providers {
		modes[i] = fmt.Sprintf("--mode %s for %s", agentMode(provider), provider)
	}
	return "Every sub-agent works unattended, so start each in the mode that never asks for permission: " +
		strings.Join(modes, ", ") + " (paseo run --mode <mode>). Without it, a permission prompt stalls the run."
}

// OrchestratorPrompt is the orchestrator's first message: its standing instructions, the mode its
// sub-agents start in, the run, the steps, the commands that move it, the workflow contract, and
// the first step.
func OrchestratorPrompt(name string, d Definition, v Vars) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\nThis is run %s of the workflow “%s”. You are its orchestrator", strings.TrimSpace(d.Orchestrator.Instructions), subAgentModes(), v.Run, name)
	if v.Repo != "" {
		fmt.Fprintf(&b, ", in a worktree of %s on branch %s", v.Repo, v.Branch)
	}
	b.WriteString(".\n\nBefore starting workers, find this agent's workspace ID with Paseo's workspace listing. Pass --workspace <id> explicitly to every worker so it uses this workspace's working directory; --cwd alone can inherit the parent's workspace. Verify each worker with paseo inspect before it edits files. Stop a misplaced worker and relaunch it in the correct workspace. Do not create a replacement worktree in another repository or fall back to a main checkout; if the task names a repository that differs from this run's project, ask Zach before proceeding")
	fmt.Fprintf(&b, ".\n\nWhat started it:\n\n%s\n\nIts steps:\n\n", strings.TrimSpace(v.Input))
	for i, s := range d.Steps {
		fmt.Fprintf(&b, "%d. %s (%s, id %s)\n", i+1, s.Title, s.Kind, s.ID)
	}
	fmt.Fprintf(&b, "\nYou get one step at a time. Move the run with these commands:\n\n"+
		"  intelligence workflow next --run %[1]s --summary \"<one line>\"    this step is done; prints the next one\n"+
		"  intelligence workflow back --run %[1]s <step id> --why \"<what is missing>\"    send the work back to an earlier step\n"+
		"  intelligence workflow skip --run %[1]s --why \"<why>\"    skip a step its instructions let you skip\n"+
		"  intelligence workflow ask --run %[1]s --note note.md    ask Zach (Markdown); then end your turn: his reply comes as your next message\n"+
		"  intelligence workflow fail --run %[1]s --why \"<why>\"    the run cannot be done\n\n"+
		"next after the last step finishes the run.\n\n", v.Run)
	b.WriteString("Workflow instructions take precedence over conflicting shared skill defaults (including start-here, agent-factory and paseo-cli); " +
		"keep system, host, repository and user constraints. Only the orchestrator moves the run. " +
		"Pass each sub-agent its role, current step, run id, task context and limits, including this workflow contract. " +
		"Follow this workflow's model choices rather than a shared skill's default.\n\n" +
		"Builders may edit, test and commit in the worktree; do not push, land, deploy or change external systems before the step that authorizes it. " +
		"Uploading images and Markdown files to files.zachlatta.com to show Zach is an exception to this external-change limit: follow the file-delivery rule below in any step. Checkers follow their step's edit limits. Shared skill writes and queued mutations are external changes too: " +
		"pass proposed edits and evidence to the landing step. " +
		"The landing step also applies the run's approved external changes (skill edits, queued mutations). " +
		"Preserve scratch needed by later steps. A worker reports back and ends its turn; the orchestrator owns reuse and archiving. " +
		"After a successful ask, end your turn and wait for the reply message; do not poll or create a wakeup.\n\n")
	b.WriteString(StepMessage(d, 0, v))
	return b.String()
}

// StepMessage is step i's title and filled instructions, as the orchestrator gets them.
func StepMessage(d Definition, i int, v Vars) string {
	s := d.Steps[i]
	return fmt.Sprintf("Step %d of %d: %s\n\n%s", i+1, len(d.Steps), s.Title, strings.TrimSpace(v.fill(s.Instructions))+"\n\n"+UnattendedShellRule+"\n\n"+FileDeliveryRule)
}

// FileDeliveryRule is appended at runtime so saved workflows get it too. Uploading belongs
// on the agent's host: the server cannot read its files or decide which artifacts to publish.
const FileDeliveryRule = `When showing Zach images or Markdown files in notes, asks, step summaries or chat reports, host them on https://files.zachlatta.com at public, unguessable URLs with no authentication. These delivery uploads are allowed in any step, including sign-off, and are not deferred to landing, unless a step explicitly forbids uploads. This explicit delivery preference overrides the file-share skill's restricted-delivery default, with the private-data exception below. Read the file-share skill for the current upload API.

For public files, omit passwords and allowed_emails (or emails); omit namespace on the first upload so the service generates it. Do not invent a URL or reuse a predictable namespace. Check the returned SHA-256 against the local bytes and verify an anonymous GET returns 200 with the expected content type before linking it. Use ![description](https://...) for images and [report](https://...) for Markdown. Upload Markdown as text/plain; charset=utf-8 so browsers open it inline, and upload and rewrite any local image or Markdown references inside the report too.

Files containing donor, contact, financial or student data must stay restricted to Zach's verified email address from the file-share skill unless Zach explicitly asks to publish that data. For these private files, set allowed_emails (or emails) to that address, verify the SHA-256 and that an anonymous GET returns 401, and link them as text rather than embedding images. This exception also applies to private files referenced inside a report; never publish private data through a public parent report.

Never send local paths as deliverables. Pass this file-delivery rule to every worker. If the current step explicitly forbids uploads, preserve the files and pass their paths and this rule to an authorized step. If upload or access verification fails, report the failure and keep the files for retry; do not claim they were delivered.`

// OrchestratorInstructions are the starting workflow's standing instructions to its orchestrator.
const OrchestratorInstructions = `You run a workflow. You never do the work yourself: for each step, start sub-agents to do it, read what they report, and decide when the step is done.

Start a sub-agent in this workspace with the paseo CLI, for example:

  paseo run --no-wait --title "<what it does>" --provider claude/claude-opus-5-5 --mode bypassPermissions "<its task, with all the context it needs>"

Then paseo wait <id> until it is done, and paseo logs <id> to read what it did. paseo send <id> "<message>" gives a sub-agent more work and keeps its context: use it when work comes back to a sub-agent that did it. A checker must be a fresh sub-agent that did none of the work. Archive sub-agents you are done with (paseo archive <id>).

Use frontier models, and spread the work across the plans: claude/claude-opus-5-5, codex and antigravity/gemini-4-carbon-high (Gemini 4 Carbon).`

// Starting is the workflow an account starts with (its Host set to one of the account's): a task from the Workflows page, its project
// found, the work, three gates, Zach's sign-off for broader changes, and landing.
var Starting = Definition{
	Trigger: TriggerTask,
	Project: Project{Mode: ProjectMatch, Provider: "claude", Model: "claude-opus-5-5", Thinking: "medium",
		Instructions: "Pick the repository this task belongs in. Read each candidate's README and recent commits."},
	Orchestrator: Orchestrator{Provider: "claude", Model: "claude-opus-5-5", Thinking: "high", Instructions: OrchestratorInstructions},
	Steps: []Step{
		{ID: "build", Kind: KindStep, Title: "Do the work", Instructions: "Have sub-agents do this:\n\n{{input}}\n\n" +
			"Follow the repository's AGENTS.md, and work test-first if the repository does. Move on when it is committed and the tests pass."},
		{ID: "meets", Kind: KindGate, Title: "Meets the request", Instructions: "Have a fresh sub-agent that did none of the work check that the change does what Zach asked, " +
			"including what he left unsaid. It tests it itself: the tests, the dev server, a browser for UI.\n\n" +
			"If something is missing or broken, send it back to Do the work with what is missing. Otherwise move on."},
		{ID: "fit", Kind: KindGate, Title: "Fits the codebase", Instructions: "Have a fresh sub-agent read the diff against the repository's conventions. " +
			"Is it test-first where the repository is? Is anything now redundant? Did a small refactor show a larger one we should do? " +
			"Does the codebase evolve with the change, or is it bolted on?\n\nSmall fixes: have them made here. Anything bigger: send it back to Do the work."},
		{ID: "main", Kind: KindGate, Title: "Up to date with main", Instructions: "Rebase on origin/main and run the tests. " +
			"If something conflicts or fails, have it fixed, then check again. Move on when it is current and green."},
		{ID: "signoff", Kind: KindSignoff, Title: "Zach's sign-off", Instructions: "If the change is only what Zach asked for, with no broader architecture change " +
			"(new tables, dependencies, contracts or boundaries), skip this step.\n\nOtherwise write him a note in Markdown: the original task, " +
			"the architecture change before and after, before and after screenshots, and anything he needs to decide, with your recommendation. " +
			"Ask, then do what his reply says."},
		{ID: "land", Kind: KindStep, Title: "Land and deploy", Instructions: "Rebase on origin/main, land on main the way the repository's AGENTS.md says, " +
			"and wait until the deploy reports the new version. " +
			"The landing step also applies the run's approved external changes (skill edits, queued mutations). " +
			"Then move on, which finishes the run."},
	},
}

// UnattendedShellRule is included for saved workflows too, not only Starting.
const UnattendedShellRule = "Agents run unattended. Pass this shell guidance to each sub-agent. " +
	"Even bypass mode can ask for permission on shell safety checks. Intelligence denies those prompts and asks for a safer approach. Use Write/Edit tools for prompt files and other multiline content instead of shell heredocs; split compound commands and use git -C instead of cd && git. A denial does not authorize broader actions. If blocked, report why."
