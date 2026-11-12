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
	Priority     string           `json:"priority,omitempty"`  // empty is Automatic, resolved from Trigger when each run starts
	Host         string           `json:"host"`                // the account's host every run happens on
	Trigger      string           `json:"trigger"`             // task (submitted on the Workflows page), manual, email or voice_memo
	Email        *EmailFilter     `json:"email,omitempty"`     // email: which email starts a run
	VoiceMemo    *VoiceMemoFilter `json:"voiceMemo,omitempty"` // voice_memo: which new PDW recording starts a run
	Project      Project          `json:"project"`
	Orchestrator Orchestrator     `json:"orchestrator"`
	Steps        []Step           `json:"steps"`
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
	PriorityNormal    = "normal"
	PriorityImmediate = "immediate"
	TriggerTask       = "task"
	TriggerManual     = "manual"
	TriggerEmail      = "email"      // an email to the account's connected Gmail matches Email
	TriggerVoiceMemo  = "voice_memo" // a new PDW voice memo recording matches VoiceMemo
	ProjectMatch      = "match"
	ProjectFixed      = "fixed"
	KindStep          = "step"
	KindGate          = "gate"
	KindSignoff       = "signoff"
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
	if err := d.Validate(); err != nil {
		return d, err
	}
	return d.normalized(), nil
}

func (d Definition) normalized() Definition {
	if d.Project.Mode == ProjectFixed && d.Project.Directory != "" {
		d.Project.Directory = path.Clean(d.Project.Directory)
	}
	return d
}

// EffectivePriority resolves Automatic without changing the saved configuration.
func (d Definition) EffectivePriority() string {
	if d.Priority != "" {
		return d.Priority
	}
	if d.Trigger == TriggerEmail || d.Trigger == TriggerVoiceMemo {
		return PriorityImmediate
	}
	return PriorityNormal
}

// Validate says what in d cannot run.
func (d Definition) Validate() error {
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	switch {
	case d.Priority != "" && d.Priority != PriorityNormal && d.Priority != PriorityImmediate:
		return errors.New("priority must be normal or immediate")
	case d.Host == "":
		return errors.New("a workflow needs a host to run on")
	case !hostclient.ValidName(d.Host):
		return fmt.Errorf("host %q is not a host's name", d.Host)
	case d.Trigger != TriggerTask && d.Trigger != TriggerManual && d.Trigger != TriggerEmail && d.Trigger != TriggerVoiceMemo:
		return fmt.Errorf("trigger %q: want %s, %s, %s or %s", d.Trigger, TriggerTask, TriggerManual, TriggerEmail, TriggerVoiceMemo)
	case d.Trigger == TriggerVoiceMemo && d.VoiceMemo == nil:
		return errors.New("a voice memo trigger needs a voiceMemo filter (use {} for all recordings)")
	case d.Trigger != TriggerVoiceMemo && d.VoiceMemo != nil:
		return errors.New("only a voice memo trigger has a voiceMemo filter")
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
// repositories, routing guidance (including for saved definitions), and how to answer.
func ProjectPrompt(run string, d Definition, input string, repos []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nThe task:\n\n%s\n\n", strings.TrimSpace(d.Project.Instructions), strings.TrimSpace(input))
	if len(repos) == 0 {
		b.WriteString("This host has no repositories in Paseo yet.\n\n")
	} else {
		fmt.Fprintf(&b, "The repositories on this host (their main checkouts):\n\n- %s\n\n", strings.Join(repos, "\n- "))
	}
	b.WriteString(projectRoutingGuidance + "\n\n" + ProjectRule + "\n\n")
	fmt.Fprintf(&b, "Change nothing. When you have decided, run one of these, then stop:\n\n"+
		"  intelligence workflow project --run %[1]s <directory>\n"+
		"  intelligence workflow project --run %[1]s none\n\n"+
		"none means the task belongs in no repository: the orchestrator then works in the home directory.", run)
	return b.String()
}

const projectRoutingGuidance = `Choose by the requested work product, not by topic overlap or where source data lives.
- Honor an explicit workspace choice in the task or workflow instructions. Mentioning a data source or a program name alone is not a workspace choice.
- For one-off reports, analyses, decks, emails, research and similar deliverables, choose desk when it is among the candidates. desk is the general workspace for this work, even when it uses data or tools from another repository. Read desk's README before ruling it out.
- Reading or querying a warehouse does not make its code repository the task's workspace. For example, a deck about program growth using warehouse data belongs in desk; a fix to the warehouse's pipeline or data models belongs in the warehouse repository.
- Choose a code repository when the task calls for changes to a repository's code, tests, configuration or maintained documentation, or investigation of that repository's behavior. Do not turn a one-off deliverable into a code change merely because its subject appears in a README or recent commits.
- If desk is absent, use another candidate only if the requested work belongs there; otherwise answer none. Do not invent a checkout path.`

// ProjectRule is appended to every finder's prompt after the routing guidance, so saved workflows
// get it too. A finder that picks the repository a report's data comes from publishes the report
// when that repository is public.
const ProjectRule = "Check a candidate's visibility before you pick it (gh repo view <owner/name> --json visibility, " +
	"with owner/name from its git remote). Never pick a public repository for a result for Zach (a report, deck, analysis, " +
	"metrics, a data pull or export, research, an email or an answer), even when its data comes from that repository: " +
	"everything a run commits there is published. Such results go to a private repository."

// PublicRepoRule is in every orchestrator's contract, so saved workflows get it too, and it is
// passed to every worker: what a public repository must never get, whichever step adds it.
const PublicRepoRule = "If this run's repository is public, everything committed to it is published. Commit only changes the repository " +
	"keeps (code, configuration, documentation). Never commit a result for Zach there: reports, decks, analyses, metrics or counts " +
	"about people, programs or money, data exports (CSV, JSON, spreadsheets), work/ or scratch folders, or donor, contact, " +
	"financial or student data. Aggregate-only and no-secrets do not make internal data publishable, and a work/ folder already " +
	"on main is not a convention to follow. Keep such output in .factory-scratch/ (git-ignored) and deliver it to Zach privately. " +
	"Keep files.zachlatta.com URLs to private deliverables out of public repositories, issues and pages. " +
	"This rule governs what enters public Git or is posted publicly. For an outgoing Git finding, fix only the Git contents. " +
	"Never use this rule to edit, re-upload or change access to a hosted deliverable. " +
	"Before any push, review every outgoing commit, merge commits included, not only the final diff (git log -m --stat origin/main..HEAD; " +
	"-m shows what merge commits add): a file added and " +
	"deleted in a later commit is still published with the push. If any commit adds such output, rewrite the commits to drop it first. " +
	"If the task's deliverable is a result rather than a change to this repository, the run is in the wrong repository: " +
	"ask Zach before proceeding."

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
	fmt.Fprintf(&b, ".\n\nWhat started it:\n\n%s\n\n", strings.TrimSpace(v.Input))
	fmt.Fprintf(&b, "Before starting work, summarize the whole task above as a concise action title (3–8 words, at most 80 characters). Describe the requested outcome, not just the prompt's opening words. Preserve important constraints and negations. For example: \"Summarize workflow run titles\". Save it with:\n\n  intelligence workflow summarize --run %s --summary \"<short task summary>\"\n\nUse proper shell quoting for your text. This only labels the run and does not advance a step. If saving the summary fails, continue with the workflow; do not block the task on it.\n\n", v.Run)
	b.WriteString("Its steps:\n\n")
	for i, s := range d.Steps {
		fmt.Fprintf(&b, "%d. %s (%s, id %s)\n", i+1, s.Title, s.Kind, s.ID)
	}
	fmt.Fprintf(&b, "\nYou get one step at a time. Move the run with these commands:\n\n"+
		"  intelligence workflow answer --run %[1]s --channel email --message-id <sent-id> [--thread-id <thread-id>] [--sent-at <date>] [--checked]    record an answer to the requester\n"+
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
		"Uploading images and Markdown files to files.zachlatta.com to show Zach is an exception to this external-change limit: follow the file-delivery rule below in any step. The exception also covers access-only changes requested by Zach or an authorized sharing manager for an existing artifact, unless the step forbids the change. Checkers follow their step's edit limits. Shared skill writes and queued mutations are external changes too: " +
		"pass proposed edits and evidence to the landing step. " +
		"The landing step also applies the run's approved external changes (skill edits, queued mutations). " +
		"Preserve scratch needed by later steps. A worker reports back and ends its turn; the orchestrator owns reuse and archiving. " +
		"After a successful ask, end your turn and wait for the reply message; do not poll or create a wakeup.\n\n" +
		PublicRepoRule + "\n\n")
	b.WriteString("Lifecycle: after next, skip or back, keep working in the same turn using the returned instructions. Wait for sub-agents in the foreground. End your turn only after a successful ask or after the run is finished, done or failed.\n\n")
	b.WriteString(StepMessage(d, 0, v))
	return b.String()
}

// StepMessage is step i's title and filled instructions, as the orchestrator gets them.
func StepMessage(d Definition, i int, v Vars) string {
	s := d.Steps[i]
	return fmt.Sprintf("Step %d of %d: %s\n\n%s", i+1, len(d.Steps), s.Title, strings.TrimSpace(v.fill(s.Instructions))+"\n\n"+UnattendedShellRule+"\n\n"+FileDeliveryRule+"\n\n"+DeliverableEmailRule+"\n\n"+v.fill(RunAnswerRule))
}

// DeliverableEmailRule is appended at runtime, like FileDeliveryRule, so saved workflows get it
// too. Without it a run whose result is a report finished with that report only on its run page
// whenever its landing step's own wording did not cover the run's repository. The address is the
// zrl-bot-email skill's: the repository is public and carries no personal address.
const DeliverableEmailRule = "When the run's result is a deliverable for Zach rather than landed code or a change to hosts or services " +
	"(a report, investigation, answer, proposal, plan, deck or analysis, or such a result alongside code), email it to him from the zrl-bot mailbox " +
	"as the zrl-bot-email skill says: a reply in his request thread when the run started from his email, otherwise a new email to the address the skill names for new mail. " +
	"This holds in any repository, and even when nothing lands. Put the full substance in the body (the answer first, then the findings, tables and decisions he must make), " +
	"with files.zachlatta.com links to the full report and files under the file-delivery rule above; never only a link or a local path. " +
	"Earlier steps draft the email (subject and HTML body) in the worktree and pass it on; only the last step sends it, or, when the run ends early (fail or skip to the end), a worker the orchestrator starts before it moves the run. " +
	"Before sending, search zrl-bot's sent mail from the run's request date, not a recent-days window: by subject, by the report's URL, and in the request thread if there is one. " +
	"Inspect every match, send only if none already delivered it, so it goes out once, and put the sent message's id (or the earlier delivery's) in the run's final summary, and record it with intelligence workflow answer --run <run id> --channel email --message-id <sent id> --thread-id <thread id> [--sent-at <date>] [--checked]. " +
	"Pass this deliverable-email rule to every worker."

// FileDeliveryRule is appended at runtime so saved workflows get it too. Uploading belongs
// on the agent's host: the server cannot read its files or decide which artifacts to publish.
const FileDeliveryRule = `When showing Zach images or Markdown files in notes, asks, step summaries or chat reports, host them on https://files.zachlatta.com at public, unguessable URLs with no authentication. These delivery uploads are allowed in any step, including sign-off, and are not deferred to landing, unless a step explicitly forbids uploads. This explicit delivery preference overrides the file-share skill's restricted-delivery default, with the private-data exception below. Read the file-share skill for the current upload API.

For public files, omit passwords and allowed_emails (or emails); omit namespace on the first upload so the service generates it. Do not invent a URL or reuse a predictable namespace. Check the returned SHA-256 against the local bytes and verify an anonymous GET returns 200 with the expected content type before linking it. Use ![description](https://...) for images and [report](https://...) for Markdown. Upload Markdown as text/plain; charset=utf-8 so browsers open it inline, and upload and rewrite any local image or Markdown references inside the report too.

Files containing donor, contact, financial or student data must stay restricted to Zach's verified email address from the file-share skill (or the recipients specified by the request or saved workflow) unless Zach explicitly asks to publish that data or an authorized sharing manager requests publication as described below. So must internal metrics: counts, rates or spend about people, programs or money, even when every number is an aggregate. So must every internal report, even with no metrics: audits, reviews, walkthroughs of Zach's runs, agents, workflows or incidents, and anything naming security weaknesses or credential names, even redacted. For these private files, set allowed_emails (or emails) to the specified verified recipients, verify the SHA-256 and that an anonymous GET returns 401, and link them as text rather than embedding images. This exception also applies to private files referenced inside a report; never publish private data through a public parent report (an authorized make-public request excepted).

Honor access settings and recipient lists specified for initial delivery. An authorized request to make an existing link public publishes its current contents, including private material. Zach can authorize publication. For others, sharing authority must come from Zach's instructions or the owning workflow, including a workflow that creates collateral for their role to share externally. Honor any workflow sign-off gate. Viewing access alone is insufficient. No request to make an existing link public permits redacting, trimming, rewriting, swapping maps or media, rebuilding or re-uploading. If authority is missing or sign-off is pending, leave the files and access untouched. Ask Zach whether to open the existing link as-is when his decision is needed. On authorized publication, clear password and allowed_emails only on the covered files. Verify unchanged URLs, versions and SHA-256 hashes and anonymous GET 200. Put “Public link. Anyone with the URL can open this file.” beside each public link in the reply to the requester, including initial public deliveries. Keep private deliverable URLs out of public repositories, issues and pages.

Sharing-policy review must never redact, trim, rewrite, rebuild, swap maps or media, or re-upload content to satisfy a restriction. If public access lacks authorization under the defaults, correct only allowed_emails on the existing bytes, honoring the specified recipients. If public access is authorized, preserve it and at most add a missing notice to the message.

Never send local paths as deliverables. Pass this file-delivery rule to every worker. If the current step explicitly forbids uploads, preserve the files and pass their paths and this rule to an authorized step. If upload or access verification fails, report the failure and keep the files for retry; do not claim they were delivered.`

// OrchestratorInstructions are the starting workflow's standing instructions to its orchestrator.
const OrchestratorInstructions = `You run a workflow. You never do the work yourself: for each step, start sub-agents to do it, read what they report, and decide when the step is done.

Start a sub-agent in this workspace with the paseo CLI, for example:

  paseo run --no-wait --title "<what it does>" --provider claude/claude-opus-5-5 --mode bypassPermissions "<its task, with all the context it needs>"

Then paseo wait <id> until it is done, and paseo logs <id> to read what it did. paseo send <id> "<message>" gives a sub-agent more work and keeps its context: use it when work comes back to a sub-agent that did it. A checker must be a fresh sub-agent that did none of the work. Archive sub-agents you are done with (paseo archive <id>).

Use frontier models, and spread the work across the plans: claude/claude-opus-5-5, codex and antigravity/gemini-4-carbon-high (Gemini 4 Carbon).`

// Starting is the workflow an account starts with (its Host set to one of the account's): a task from the Workflows page, its project
// found, the work, three gates, Zach's sign-off for broader changes, a public repository check, and landing.
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
		{ID: "public", Kind: KindGate, Title: "Public repo check", Instructions: "Check the repository's visibility " +
			"(gh repo view <owner/name> --json visibility). If it is private, skip this step.\n\n" +
			"If it is public, have a fresh sub-agent review every outgoing commit, not only the final diff: list them with " +
			"git log -m --stat origin/main..HEAD (-m includes merge commits, which git log otherwise leaves out) and read each file " +
			"each commit adds or changes, including reports outside work/. " +
			"A file deleted in a later commit is still published with the push. " +
			"Send the work back to Do the work if any commit adds a result rather than a change the repository keeps: a report, deck, " +
			"analysis, metrics or counts about people, programs or money, a data export (CSV, JSON, spreadsheet), a work/ or scratch " +
			"folder, or donor, contact, financial or student data; Do the work must rewrite the commits to drop it. " +
			"Flag any files.zachlatta.com link to a private deliverable, including in documentation. Remove it from every outgoing commit that introduces it. " +
			"Keep those links out of public issues and pages too. This gate reviews Git contents only. Never send a hosted deliverable back for edits, re-upload or access changes. " +
			"Aggregate-only and no-secrets do not make internal data publishable. " +
			"It also checks for secrets, hostnames, private addresses, home-directory paths and personal emails, and fixes small ones here. " +
			"Move on when no outgoing commit adds anything private."},
		{ID: "land", Kind: KindStep, Title: "Land and deploy", Instructions: "Rebase on origin/main, land on main the way the repository's AGENTS.md says, " +
			"and wait until the deploy reports the new version. " +
			"The landing step also applies the run's approved external changes (skill edits, queued mutations). " +
			"Then move on, which finishes the run."},
	},
}

// UnattendedShellRule is included for saved workflows too, not only Starting.
const UnattendedShellRule = "Agents run unattended. Pass this shell guidance to each sub-agent. " +
	"Even bypass mode can ask for permission on shell safety checks. Intelligence denies those prompts and asks for a safer approach. Use Write/Edit tools for prompt files and other multiline content instead of shell heredocs; split compound commands and use git -C instead of cd && git. A denial does not authorize broader actions. If blocked, report why."

// RunAnswerRule applies to saved workflows too, including code and service tasks.
const RunAnswerRule = "Record every answer sent to the requester, including requesters other than Zach, with intelligence workflow answer --run {{run}} --channel email --message-id <sent message id> [--thread-id <thread id>] [--sent-at <date>] [--checked]. " +
	"The final step must do this before finishing. On an early exit (fail or skip to the end), the authorized worker that sends the answer must record it before the orchestrator ends the run. Only a step authorized to send may send. " +
	"Use --checked only when the sent answer passed the required review. Record earlier unchecked sends too. Use the actual send time in RFC 3339 or RFC 5322 format when available; immediately after sending, omit --sent-at if the mail tool returns no timestamp, and the server defaults to now(). " +
	"For an earlier delivery, inspect the message and supply its actual send date and identifiers; do not use the current time. For another response channel, use that channel and its message identifier. Do not invent a send or record an unsent draft. If no checked answer was delivered, report that explicitly. Pass this rule to every worker."
