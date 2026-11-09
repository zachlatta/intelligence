# Intelligence

A web app, inspired by agent-factory, that will orchestrate coding agents through Paseo on a fleet
of hosts. For now it shows each host's Paseo daemon, its schedules (which it can also make, change,
pause, run and delete), and how much of each subscription plan (every Claude login, Codex, Muse,
Antigravity) is used over time, and all of its state is readable
with read-only SQL. People sign in to accounts with a password, and each account sees only its own
hosts and everything they report.

## How it fits together

One binary, five commands:

- `intelligence` (or `intelligence server`): the web app. It runs in Coolify with a Postgres
  database, reachable only on the tailnet.
- `intelligence host`: runs on each host as a systemd user service. It keeps one connection to
  the local Paseo daemon over the daemon's own WebSocket protocol (`internal/paseo`), reads its
  status, workspaces and agents, then follows the updates the daemon pushes. It holds one
  WebSocket stream to the server (below) for its reports, its agents' transcripts, its plans' usage
  and the server's commands, each handled on its own (`hostclient.Client`), so a slow transcript upload
  holds up no report. The host dials out, so it needs no inbound port, and the server never dials a host.
- `intelligence probe`: runs as root on each Linux host (a systemd system service): the eBPF
  probe (`internal/probe`) that charges every process's CPU to its Paseo agent and records OOM kills.
  The host client reads it over a Unix socket.
- `intelligence workflow`: what a workflow run's agents run on their host to move the run
  (Workflows, below). It reaches the server with the host's key.
- `intelligence version`: prints the commit the binary was built from.

### State and the read-only SQL contract

All app state is in Postgres (`internal/db/migrations/`), in the `public` schema. `POST /api/sql`
runs a query as the signed-in account's own login role, which can only SELECT, so every piece of
the account's state can be read and none written.

Each account reads only its own rows, and Postgres enforces it, not the app: every table has
row-level security, and every view is `security_invoker`, so it reads as whoever queries it.
Account *n*'s role is `<read-only role>_a<n>` (`db.AccountRoles`): a member of the read-only role
(so it can SELECT what that role can), read-only by default, and the only role whose rows
`current_account_id()` finds. That function reads `session_user`, which a query cannot change, so
`SET ROLE`, `RESET ROLE` or a setting cannot read another account's rows. Its password is derived
from the read-only role's, which only the server knows; the server sets it again on start. The
shared read-only role itself reads as no account, so it sees no account's rows. An account owns
the hosts it holds keys for (`host_keys`), and with them their workspaces, agents,
transcripts, commands, resources, and the plan logins its hosts read. The page routes read through
the same role, so a page shows an account only what its SQL would.

Before each deploy the contract is checked (`db.CheckContract`): every relation is in `public` and
readable by the read-only role, which cannot write or bypass row-level security; every table but
`schema_migrations` has a row-level security policy; every view is `security_invoker`; and every
account's role is read-only and bound by row-level security. The image build runs every test
against a throwaway Postgres, so a broken contract fails the build and nothing deploys; the server
checks it again against production when it starts.

The hierarchy is hosts > workspaces > agents:

- `hosts`: each host's latest report: whether it is connected to its daemon (`connected`, else
  why in `paseo_error`), the daemon's version, listen address, pid, start and providers. Each
  provider carries its catalog, the models (with the thinking levels each takes) and permission
  modes the daemon lists for it (`get_providers_snapshot`, re-read with the status; a provider still
  loading keeps the last), read one row each through the views `provider_models` and `provider_modes`.
- `workspaces`: each host's Paseo workspaces: a directory, local checkout or git worktree that agents
  run in (`kind`), with its status, project and its repository's main checkout (`project_root`),
  branch, diff, directory and `tags`. `listed = false` is a
  workspace Paseo no longer lists (archived or background) that still has an agent in it. Tags say
  where a workspace came from: `Paseo GUI` is one made in the Paseo app (desktop, web or mobile).
  The host reads it from the daemon's creation receipt (`~/.paseo/creations`): the app keys each
  creation `draft_…`, the CLI, MCP tools and Intelligence's commands do not. A workspace with no
  receipt (made before the daemon kept them, or opened rather than created) has no tag.
- `agents`: every unarchived agent, in its workspace (`host`, `workspace_id`), with its model,
  thinking level, status, attention, parent (for sub-agents) and times.
- `transcripts` and `transcript_entries`: each agent's transcript, its projected Paseo timeline.
  An entry keeps the daemon's item whole (`item`, jsonb); `kind` (user_message, assistant_message,
  reasoning, tool_call, ...), `tool`, `status` and `text` are generated from it. NUL characters,
  which Postgres cannot store, become U+FFFD.
- `workspace_diffs`: each git workspace's diff (below): its files, each with its hunks of lines, as
  `files` (jsonb), its totals, and why the daemon could not read it (`error`); the view
  `workspace_diff_files` reads one changed file a row.
- `commands`: what the server asked a host to do (below): its kind, args, status, result or error,
  and when it was created, sent, started, finished and when it expires.
- `schedules` and `schedule_runs`: each host's Paseo schedules and their runs (below), with the view
  `schedule_status` (each schedule's run counts and newest run). `hosts.schedules_at` and
  `schedules_error` say when the host last listed them, and why it could not.
- `host_minutes`, `agent_minutes`, `oom_kills` and `host_probes`: each host's resources over time
  (below), with the views `host_current`, `host_days` and `agent_days`.
- `usage_accounts`, `usage_readings`, `usage_active` and `usage_sources`: subscription plans' usage
  over time (below), with the views `usage_current`, `usage_pace`, `usage_cycles` and `usage_totals`.
- `accounts` (username, argon2id password hash, its role), `sessions` (signed-in browsers; a hash
  of each cookie's token), `host_keys` (each host's account and a hash of its key) and
  `host_enrollments` (hosts waiting for their code to be entered). An account reads only its own.
- `workflows`, `workflow_versions` (every saved definition), `workflow_runs` and
  `workflow_run_events` (what happened in each run, in order): Workflows, below.
- `gmail_connections` (the account's Gmail mailbox: its address, the account holder's own
  addresses, how far it is read, and its Google token, sealed with `CONNECTIONS_KEY`, so reading it
  gives nothing) and `emails` (every email it received, and what it did and why): Email triggers,
  below.
- `schema_migrations`.

Each report replaces its host's workspaces and agents. A host that cannot reach its daemon has none,
so nothing stale reads as current. The `host_status` view adds `online` (the host holds a stream
to the server, `stream_id`, which was seen within the last minute) and counts; `workspace_status`
adds each workspace's agent counts.

### The stream

A host dials `GET /api/hosts/{host}/stream` (a WebSocket, with its own key) and keeps it
open, dialing again 5 seconds after it drops. Each frame is a JSON `hostclient.Frame`:

- server to host: `hello` first (where its transcript copies end), then `command`s. The server's
  version is in the dial's `X-Intelligence-Version` header.
- host to server: `report` (on each new stream, and within a second of a change; the host re-reads
  the daemon's status every 15 seconds and reports it if it changed), `transcript` batches, `usage`
  (every 5 minutes), `host` (every minute), `schedules` (when they changed, below), `diff` (a workspace's diff, when it
  changed, below) and `command_update`s. Each carries an `id`, and the server answers each with an
  `ack` once it is stored (or with why not), transcript batches on their own so a large one holds up
  nothing else.

Each side pings the other: the host every 30 seconds (an unanswered ping, or a frame not acked in 30
seconds, drops the stream, and the host dials again) and the server every 20 seconds, recording the
host as seen. A newer stream for the same host takes it over and ends the older one, on whichever
server instance holds it.

Server instances learn of each other's changes through Postgres `NOTIFY` on the `fleet` channel
(`fleet.Changes`): a queued command, a command's update and a stream taking a host over. Nothing on
the server polls.

### Accounts and host keys

An account is a username (lowercase letters, digits, `-` and `_`) and a password of at least 12
characters, hashed with argon2id. Anyone who can reach the server can make one; an account starts
with no hosts and sees nothing until one joins it. A signed-in browser holds a random session
token in a cookie (HttpOnly, Secure, SameSite=Strict, 30 days); Postgres keeps only its hash.
Signing out ends that session; changing the password ends all of them.

There are no shared tokens. A host joins an account with a code:

1. A host with no key (`~/.config/intelligence/host.key`) asks to join
   (`POST /api/host-enrollments`), and logs a code such as `7KQ2-M9XD`, good for an hour.
2. Someone signed in enters the code on `/account` (`POST /api/host-enrollments/claim`). A host
   name another account holds cannot be claimed.
3. The host, waiting on `POST /api/host-enrollments/{id}/key` with the secret only it holds, gets
   its key at once (woken by the claim, not by polling), keeps it (mode 600) and streams with it.

A key is good for its own host name only. Removing a host from its account (`/account`) deletes its
key and everything it reported; the host's next dial is refused, so it forgets the key and asks to
join again with a new code. A host that expires a code without its being entered asks for a new one.

### Commands

The server tells a host what to do through its stream. A command is queued in `commands`
(`POST /api/hosts/{host}/commands`, below), goes down the host's stream at once if it is connected
(to any server instance; else when it next connects), and the host runs it
against its daemon and reports `running`, then `succeeded` with a result (`{"workspaceId",
"agentId"}`) or `failed` with the daemon's reason. Kinds and their args:

- `create_workspace`: `cwd`, and optionally `title`; with `worktree: true`, a new git worktree of the
  repository at `cwd`, on `branch` from `base`. With `agent` (`create_agent`'s args, without
  `workspaceId` or `cwd`), an agent is then started in the new workspace. If that agent fails, the
  command fails but its result still names the workspace it made.
- `create_agent`: `provider` (`claude`, `codex`, ...), `workspaceId` (it runs in that workspace's
  directory) or `cwd`, and optionally `model`, `thinking`, `mode`, `title`, `prompt` (its first
  message) and `labels`. The agent is labelled `intelligence.command=<command id>`.
- `rename_workspace`: `workspaceId`, `title` (`""` goes back to the derived name).
- `archive_workspace`: `workspaceId`. The host archives each of the workspace's agents first, then the
  workspace (Paseo 0.15 archives a workspace's agents itself; an older daemon left them running in a
  workspace it no longer listed), skipping one already gone. The result counts them
  (`archivedAgents`). A Paseo-made worktree is removed with it.
- `rename_agent`: `agentId`, `title`. `cancel_agent` (stops its current turn) and `archive_agent`:
  `agentId`.
- `send_message`: `agentId`, `text`, and optionally `behavior`, which says what happens to a turn
  the agent is running: `steer` (the default) adds the message to it (Paseo interrupts instead
  when the provider cannot steer), `interrupt` stops it and starts a new one, and `queue` waits
  for it to end. Paseo has no queue of its own, so the host holds queued messages: one stays
  `running` until its agent is not running and every message queued for that agent before it
  (by creation time) has gone, then starts a turn. It waits until the command expires, so give it
  a longer `ttl`; one whose agent is archived meanwhile fails. On an agent that is not running,
  all three start a turn.
- `create_schedule`: `prompt`, a cadence (`every`, a duration of at least `1m` such as `30m` or
  `1h30m`; or `cron`, five fields, with an optional `timezone`), and `agent`: `provider`, `cwd`, and
  optionally `model`, `thinking`, `mode`, `title`, `isolation` (`worktree`, a new git worktree each
  run, or `local`, the directory itself) and `archiveOnFinish` (default true). Optionally `name`,
  `maxRuns`, `expiresAt` (RFC 3339) and `runNow` (run it once now too; else an `every` schedule
  first runs one interval from now). The result names the schedule (`scheduleId`).
- `update_schedule`: `scheduleId` and whatever changes, as for `create_schedule`; a cadence replaces
  the old one whole, and the agent's fields change one by one (not its title). `name: ""`,
  `maxRuns: 0` and `expiresAt: ""` clear them.
- `pause_schedule`, `resume_schedule`, `delete_schedule` and `run_schedule`: `scheduleId`.
  `run_schedule` succeeds once the run has started, with its `runId`; the run goes on after.

Delivery is at least once: a command sent down a stream that dropped before its result arrived is
sent again when the host reconnects. Creations and messages carry the command id to Paseo as their
idempotency key; renames, archives and schedule changes are idempotent (deleting a schedule that is
gone succeeds); and a host remembers its last 1000 results, so a repeat gets the first result again
without running twice. Paseo takes no idempotency key for creating or running a schedule, so those
two rely on that memory alone: a repeat that reaches a host which restarted in between (an update)
makes a second schedule or run. A
command not finished by its expiry (10 minutes by default) is `expired`: never delivered, or
delivered with no result (it may have run). A host does not start an expired command, and a result
that arrives after expiry replaces it.

### Schedules

A Paseo schedule runs a prompt on a cadence, each time in a new agent (or, for schedules made from
inside an agent, in that agent). The daemon pushes nothing when a schedule changes, so the host
lists them with every status read (every 15 seconds), on each new stream and right after each
schedule command, and sends a `schedules` frame when they changed: the schedules whole, and the
newest 50 runs of each schedule that changed (a run's output and error cut at 64 KB). A host sends a
schedule command's change before its result, so a page that reloads when the command succeeds
shows it.

The server replaces the host's schedules with each frame and adds to the runs it keeps, so the run
history outlives what the host sends; a schedule's runs go with it. A daemon that cannot list
schedules (one older than 0.15) is recorded in `hosts.schedules_error` and the last schedules are
kept. A schedule's agent settings can hold MCP servers (with credentials), a system prompt and
provider options: those are never read. A run's agent is usually archived when it finishes, and
with it its transcript, so a run keeps the agent's id and its last message (`output`).

### Diffs

Each git workspace's diff goes up the stream when it changed, one `diff` frame per workspace: what
the workspace's work changed against its base branch (from the merge base), with uncommitted and
untracked files, as the daemon parses it for its own diff view (`checkout.diff.get`, mode `all`).
Reading a diff runs git, so the host reads a workspace's again only when its diff stat, branch or
directory moved, while an agent in it is working (every 5 seconds), once after the agent stops (for
its last edits), and on each new stream; it sends it only when it differs from what it last sent. A
line is cut after 4 KB, and past 2 MB of a workspace's files the biggest lose their hunks (status
`too_large`, counts kept). A diff the daemon cannot read is sent with its reason. The server keeps
each workspace's newest, and drops it when a host connected to its daemon no longer lists the
workspace.

### Plan usage

Every 5 minutes each host reads every provider's own meter (`internal/usage`) and sends it up its
stream as a `usage` frame. Paseo's own usage report is not used: it shows only the logged-in Claude
login and misses providers.

- Claude: `cswap list --json`, so every login cswap holds, not only the active one. A login is its
  email (two organizations of one email are two logins); cswap's number for it is kept as `slot`,
  which can change. The reading's time is cswap's `usageFetchedAt`: cswap caches.
- Codex: the subscription meter (`chatgpt.com/backend-api/wham/usage`) with the login's own token
  from `~/.codex/auth.json`, sent nowhere else; when the meter fails, the newest rate-limit reading in
  the local sessions, noted as such.
- Muse: `muse usage --json`. Before the first use in a window it reports no usage; that is stored as
  a note, not as a reading.
- Antigravity: `agy -p /usage --output-format json` (no agent turn). Each model group is a pool:
  `gemini/7d`, `gemini/5h`, `claude-gpt/7d`, ...

A tool missing from a host is skipped; a failed read is recorded in `usage_sources` and stores no
reading. The same login read from several hosts is one login. Windows are `7d`, `5h` and pools'
(`fable/7d`); reset times are rounded to the minute, as providers jitter them.

`usage_readings` keeps the history as runs: a row is one window holding one value from
`first_observed` to `last_observed`, and a new row starts only when the value or reset changes. A
reading older than the newest is ignored (another host's older cswap cache), and a window still at
0% whose reset keeps moving (not started) stays one run. `usage_active` records which login each
host's tool used, and when (cswap switches Claude logins).

The views: `usage_current` (each window's latest value; `remaining_pct` is null once the window
has reset with no reading since, and `stale` after 15 minutes without one), `usage_pace` (% left
minus % of the window still to run: positive is under-spending), `usage_cycles` (the most of each
window used before each reset, so how much of each week each login used) and `usage_totals`
(logins, and weeks of quota left across them, per provider).

### Host resources

Every host measures itself (`internal/resources`) and sends each finished minute up its stream as a
`host` frame; minutes the server could not take are kept (up to three hours) and sent with the next.

- The host, every 5 seconds: CPU (busy %, from `/proc/stat`), load, memory and swap
  (`/proc/meminfo`), the home filesystem's space, pressure (`/proc/pressure`: the share of time
  tasks waited for CPU, memory or I/O), the kernel's OOM kill count (`/proc/vmstat`) and the Paseo
  daemon's cgroup (all its agents: memory less page cache, its peak, its OOM kills). A minute keeps
  the average and peak CPU, the least memory and disk available, the most pressure, and the kills.
  macOS hosts report CPU, load, memory, swap and disk (`sysctl`, `vm_stat`, `iostat`).
- Each agent, every 15 seconds: its processes are those whose environment names it
  (`PASEO_AGENT_ID`, which commands inherit); a minute keeps their most memory (proportional set
  size, so shared pages count once) and how many there were.
- Each agent's CPU and commands, and every OOM kill, from the eBPF probe where it runs (below);
  otherwise CPU comes from the processes alive at each 15-second walk, which misses short-lived
  commands (`cpu_source` says which).

The server keeps 90 days of minutes (OOM kills are kept). `host_current` is each host's newest
minute; `host_days` and `agent_days` sum days.

### The eBPF probe

`intelligence probe` runs as root on each Linux host (`deploy/install-probe`). It loads four small
programs, written with `cilium/ebpf`'s assembler (no C toolchain), on kernel tracepoints whose field
offsets it reads from the kernel at load time:

- `sched_switch`: charges each CPU's time since its last switch to the process switched away from,
  when that process belongs to an agent. Exact, and it counts every process, however short.
- `sched_process_fork`: a new process or thread belongs to its parent's agent (and a pid reused by
  a process with no agent is cleared).
- `sched_process_exec`: counts each program an agent's process starts; a program started by a
  process with no agent is passed to user space, which reads its environment for `PASEO_AGENT_ID`
  (so an agent the Paseo daemon starts is found at once). A scan every 5 seconds catches the rest.
- `oom:mark_victim`: each process the kernel chooses to kill for memory, its agent and its memory.

So a process belongs to the agent whose process tree started it. The probe serves its counters on
`/run/intelligence/probe.sock`, readable by the host user only; it holds no token. Checked on a
host: an agent's 200 short commands and CPU burn were charged within 1% of the kernel's own
accounting, and an OOM kill in a 64 MB scope was recorded with its agent.

The probe follows the server's version like the host client: every minute it asks the server's
version and, when it differs, downloads the server's binary, checks its sha256, checks that it runs
the probe (`<binary> probe version`, so a server rolled back to before the probe never replaces
it), replaces `/usr/local/lib/intelligence/intelligence` and re-executes. Its trust is the host
client's: the client already runs the server's binary as the host user. The unit limits it to
the capabilities it needs (BPF, perfmon, ptrace read of other processes' environment, memlock).

### Workflows

A workflow is the host it runs on, a trigger, a way to find the project, an orchestrator, and steps
(`internal/workflow`). Every run of it happens on its host, which must be one of the account's. Each save is a new version (`workflow_versions.definition`); a run keeps the
version it started with. A run:

1. **Finds its project.** Before anything runs in a worktree, an agent (the definition's project
   finder, started in the host's home directory, `~`) is told the task and the host's repositories
   (its workspaces' `project_root`s) and answers with `intelligence workflow project --run <id>
   <directory>|none`. A definition can name its repository instead (`"mode": "fixed"`).
2. **Starts the orchestrator in the worktree.** The server archives the finder and queues
   `create_workspace`: a git worktree of the repository on branch `wf-<run>-<title>` (or the home
   directory, for none) with the orchestrator in it. Its first message is its standing
   instructions, the input, the list of steps, the commands below, the workflow contract and
   the first step.
   The contract sets role and model handoffs, limits work to the assigned step, and assigns
   approved external changes to landing.
3. **The orchestrator moves through the steps.** It never does the work itself: it starts
   sub-agents for it (`paseo run` in its workspace, so they are its sub-agents), and decides when a
   step is done. Every agent of a run works unattended, so each starts in its provider's mode that
   never asks for permission (the one its paseo catalog marks unattended): `bypassPermissions`
   for Claude, `full-access` for Codex, `allowAll` for Muse, `full-access` for Antigravity. The
   editor offers these providers (`workflow.Providers`); one typed in by hand keeps its default.
   The server starts the finder and the orchestrator in it; `paseo run` would start a sub-agent in
   the provider's default mode, which may ask, so the orchestrator's first message tells it to pass
   `--mode` (in the prompt, not the saved instructions, so every version gets it). A step is only
   instructions to it; its kind (`step`, `gate` or `signoff`) changes how it reads, not what the
   server does. The orchestrator moves the run with:
   - `intelligence workflow next --run <id> --summary "..."`: the step is done; prints the next
     step's instructions (after the last step, the run is finished).
   - `back --run <id> <step id> --why "..."`: back to an earlier step, with what is missing.
   - `skip --run <id> --why "..."`, `done --run <id>` and `fail --run <id> --why "..."`.
   - `ask --run <id> --note note.md`: the run waits, showing Zach the note (Markdown) on its page.
     His reply (typed or dictated there) goes to the orchestrator as a message, which decides what
     it means and moves on.

The server only records each move (`workflow_run_events`), checks it can happen (a run waiting for
a reply cannot move, a canceled one says so), and answers with what the agent reads next. A move
is a `POST /api/workflow-runs/{id}/moves` with the run's host's key; the CLI reads the server and
key as the host client does (`HOST_SERVER` from `~/.config/intelligence/host.env`, the key from
`host.key`, or their `devbox` names). The run follows the commands it queued (the finder, the
workspace) through their results: a failed one fails the run.

Variables in instructions: `{{input}}`, `{{repo}}`, `{{branch}}` and `{{run}}`.

### Email triggers

A workflow's trigger can be email (`internal/email`). An account connects one Gmail mailbox with
Google's sign-in (read-only scope, offline, PKCE; the state is signed and names the account), and
the server reads it every 15 seconds: Gmail's history since the last read, then, once, the last 30
days, kept as `before` for the editor's preview. Each new email is stored in `emails` with what it
did, in the transaction that does it, so it does it once:

1. **In the thread of a run** (an earlier email in its Gmail thread started one), the workflow's
   `replies` decides: `run` (the default) adds it to the run as a message to its orchestrator; if
   the run waits on a question and the email is from one of the account holder's own addresses,
   it is the answer; if the run is over, it starts a new run of the workflow. `new` starts a new
   run; `ignore` does nothing.
2. **Otherwise** it starts the workflow whose filter (`definition.email`: `from` addresses or
   `@domain`s, `to`, `subject`, `words`, `without`, `attachment`) it matches, like a Gmail filter:
   every filled field must match. When two match, the narrower wins (more fields; an address beats
   its domain). Saving a filter that another workflow already has, sharing a sender with nothing to
   tell them apart, is refused.

A sender counts only if Gmail verified it: the first `Authentication-Results` (Gmail's own) must
say `dmarc=pass` for the From domain. Forged mail never starts or steers a run; nor does a mailing
list or newsletter (`List-Id`, `List-Unsubscribe`, `Precedence: bulk`), nor the mailbox's own mail.
A run's input is the email (its subject is the run's title), marked, unless it is from the account
holder, as information, not instructions. The mailbox is only read: nothing is labeled or sent.

### Transcripts

After each report, and every 2 seconds while an agent is running, the host syncs the transcripts of
its loaded agents (running, idle, error) whose activity changed: the whole timeline the first time,
then only what follows the server's copy (re-reading the last entry, which may still be growing), in
batches of at most 4 MB over the stream. It takes where the server's copies end from each stream's
hello, so a restart or a reconnect resumes. A new epoch (the daemon replaced the
timeline) replaces the copy.

**A closed agent's transcript is never read:** the daemon answers a timeline read by loading the
agent, which starts a closed agent again. An agent's transcript is copied while it is loaded and
kept after Paseo closes it; one closed before syncing reached it has none until it is opened again.
A transcript goes when its agent is gone from a connected host's report (archived); a host that loses
its daemon keeps its transcripts.

### Talking to Paseo

`internal/paseo` speaks the daemon's protocol directly: a WebSocket at `ws://<listen>/ws` from
`~/.paseo/paseo.pid`, a hello authenticated with `~/.paseo/local-credential`, then
`daemon.get_status`, `fetch_workspaces` and `fetch_agents` requests (paged, the first page
subscribing), and pushed `workspace_update` and `agent_update` messages. Commands use
`workspace.create`, `agent.create`, `workspace.title.set`, `archive_workspace`, `update_agent`,
`send_agent_message`, `cancel_agent` and `archive_agent`, diffs with `checkout.diff.get`, and for schedules `schedule/list`,
`inspect`, `create`, `update`, `pause`, `resume`, `delete` and `run-once` (which the daemon answers
only when the run ends, so the host watches `inspect` for the run instead); a refusal comes back as
the daemon's error. The hello opts in to
explicit subscriptions; without that the daemon streams every running agent's timeline. It decodes
only the fields stored here: agent records can carry credentials in other fields. The protocol is
not published as stable; `internal/paseo/paseotest` is a fake daemon that validates requests the
way Paseo 0.15 does, and the client was checked against a live daemon.

### Self-update

Every response carries `X-Intelligence-Version`, the server's commit. The image builds the host
binary for linux and darwin (amd64 and arm64) at that commit and serves it at
`/api/host/binary/{os}/{arch}` with its sha256. When a host sees a version other than its own
as it dials its stream (a refused dial too, so a host can update to a server it cannot talk to), it
downloads that binary (at most once a minute), checks the sha256 and that the binary runs and
reports that version, replaces its own executable and re-executes itself. A deploy restarts the
server, which drops every stream, so each host sees the new version within seconds. A host
follows the server both forward and back, so a rollback rolls the hosts back too.

## Develop

```sh
make test   # gofmt, go vet, go test -race against a throwaway Postgres in Docker
make run    # http://localhost:8080 (needs DATABASE_URL and READONLY_DATABASE_URL)
```

Tests talk to a fake Paseo daemon (`internal/paseo/paseotest`). Database tests get their own
database and read-only role (`internal/dbtest`) on the server in `TEST_DATABASE_URL`, which
`make test` sets when it is empty.

### Public repository

This repository is public. Two gates keep private details out of it:

- `internal/hygiene` (in `make test`, so in CI and the image build) fails on any file that holds a
  private or tailnet address, a tailnet name, a home-directory path, an email outside `example.com`,
  a private key or a token. Tests use `192.0.2.0/24`, `/home/someone` and `someone@example.com`.
- CI runs gitleaks over every commit on every push and pull request.

On GitHub, `main` cannot be force-pushed or deleted, secret scanning with push protection is on,
workflows get a read-only token and no secrets, and pull requests from outside contributors need
approval before CI runs.

## Routes

Every page and read route needs a signed-in account and shows only its own hosts: a page sends a
browser that is not signed in to `/login`, and an API route answers 401. Only `/healthz`, `/login`,
the account and enrollment routes, the host binaries and the streams (with a host key) are open.

- `GET /`: a collapsible tree of hosts > workspaces (with their project) > agents (with their status),
  with links to make a workspace on each host and start an agent in each workspace, and an Archive
  button on each workspace (with its agents) and each agent, through the commands API. Archiving
  that would stop a running agent, or remove a worktree with uncommitted changes, asks again: the
  button turns red and says why, and a second click within 5 seconds archives.
  Hosts start open, and so do workspaces with an agent running or needing attention; workspaces without
  agents are folded under a count. Problems (Paseo not connected, a client not on the server's version)
  show under the host. The tree refreshes in place every 15 seconds and keeps what was opened or closed.
  Above it, Plans: every login with each window's % left, its reset, pace and the hosts using it.
- `GET /schedules`: every schedule on each of the account's hosts: what it runs, when (in words for
  common cron expressions), its next run in its own time zone, its newest run and run counts, with
  Pause or Resume, Run now and Delete, and a link to make one on each host.
- `GET /hosts/{host}/schedules/{id}`: one schedule: its agent, directory, isolation, cadence,
  limits and prompt, every run kept (status, start, duration, error, output, and the transcript
  while its agent is on the host), and the form that edits it (sending only what changed).
- `GET /hosts/{host}/schedules/new`: the form that makes a schedule: a name, the prompt, cron (with
  the browser's time zone) or every, the directory (the host's repositories and its schedules'
  directories), the agent (Opus 5.5, medium thinking, `bypassPermissions` by default), a new git
  worktree each run or the directory, archive on finish, a run limit, an end, and run once now.
  Each of these pages sends its commands to the commands API and follows them through `/api/sql`.
- `GET /usage?days=7` (1 to 365): every login with a chart of each window's used % over the days,
  and its weekly peaks.
- `GET /api/usage`: the logins and their windows, as JSON.
- `GET /resources?hours=24` (1 to 2160; it was `/hosts`, which sends here), the Host monitoring tab: each host's CPU and
  memory charts, its OOM kills and its agents ranked by CPU, with their most memory and commands
  started. The home page shows each host's CPU, memory, disk and pressure under its name, and each
  agent's memory beside it.
- `GET /api/hosts/resources`: each host's newest minute, probe and agents' memory, as JSON.
- `GET /hosts/{host}/workspaces/{ws}/agents/new`: the form that starts an agent in a workspace.
  `GET /hosts/{host}/workspaces/new`: the form that makes a workspace with an agent in it, by
  default a new git worktree of one of the host's repositories (its workspaces' `project_root`s), or
  of a directory typed in. The home page links to both. Each asks what the agent should do, an
  optional title, and the provider (those the host reports available), model, thinking and mode,
  which start as Opus 5.5, medium thinking, `bypassPermissions`. Model and mode are picked from the
  provider's catalog, with "other…" for a name it does not list, and thinking offers the chosen
  model's levels; a provider whose daemon lists no catalog takes free text. The schedule forms do the
  same. The page sends a command to the
  commands API, follows it through `/api/sql`, then opens the new agent's transcript; a failure
  shows Paseo's reason. The prompt takes dictation as the chat box does (below): ⌘D or Enter again
  in an empty prompt starts the agent.
- `GET /login`: signs a browser in, or makes an account, through the account APIs.
- `GET /account`: the account's hosts (online, when each joined and last used its key), how to
  install one, the form that adds a host by its code, and the password form.
- `GET /hosts/{host}/agents/{id}`: one agent's transcript as turns: messages rendered as Markdown
  (raw HTML shown as text, unsafe links dropped, images named rather than loaded), thinking folded to
  a line, and runs of tool calls as a compact list that opens per call (commands with exit codes,
  diffs, reads, writes, searches). Long output is cut, with a link to the whole entry. Search,
  thinking and tool toggles, and Archive (asking again while the
  agent is running), which goes back to `/` once the agent is archived. While the agent is loaded the
  page re-fetches from its last block every 4 seconds and replaces it, so a growing message updates
  in place. A chat box at the bottom sends the agent a message through the commands API: Enter
  (or Steer) steers its running turn, Queue (Alt+Enter) waits for the turn to end, and Interrupt
  (Cmd/Ctrl+Enter) stops it; Shift+Enter is a new line. Messages not yet delivered are listed
  above it, read through `/api/sql`; one that fails says why and comes back into the chat box.
  With `ASSEMBLYAI_API_KEY` set, ⌘D (or Ctrl+D, or the mic button) dictates: the browser streams the
  microphone to AssemblyAI's streaming speech-to-text (Universal-3.6 Pro) and the words appear in the box
  as they are said. With text already in the box they go in at the cursor and stay there. In an empty
  box, ⌘D or Enter again stops and sends them, Edit stops and leaves them to change, Esc drops them.
  On the left, a sidebar of every agent in the workspace (this one marked, sub-agents indented, with
  their status), re-read every 15 seconds, and a link to start another there. On the right, for a git
  workspace, its diff: each changed file with its counts, opening to its lines with old and new
  line numbers, read from `workspace_diffs` whenever the host sends a new one (checked every 5
  seconds). Each panel folds away (its button in the toolbar, or `[` and `]`) and stays as it was
  left; on a narrow screen they lie over the page and start closed. Both read through `/api/sql`.
  Under its title, the agent's resources from `agent_minutes` and `oom_kills`: its CPU time (average
  and peak cores), memory now and at its peak, most processes, commands started and OOM kills, opening to charts of its CPU and memory over its life; re-read every minute while live.
- `GET /hosts/{host}/agents/{id}/resources`: those resources alone, as HTML, for the live page.
- `GET /hosts/{host}/agents/{id}/blocks?from={seq}`: the blocks from a seq on, as HTML, for the live
  page (`X-Epoch`, `X-Agent-Status`).
- `GET /hosts/{host}/agents/{id}/entries/{seq}`: one entry whole.
- `GET /api/hosts/{host}/stream`: a host's stream (WebSocket). Needs `Authorization: Bearer
  <the host's key>`, a key for that host name; else 401.
- `POST /api/hosts/{host}/commands` with `{"kind": "...", "args": {...}, "ttl": "10m"}`: queue a
  command (kinds above; `ttl` from 1s to 24h) for one of the account's hosts. 202 with the command
  and its `Location`; 400 for a kind or args that do not parse, 404 for a host that is not the
  account's.
- `GET /api/hosts/{host}/commands/{id}`, optionally `?wait=30s` (at most 1m): the command, after
  waiting until it finishes or the wait is up (woken by its update, not polling). 404 for another
  account's host.
- `GET /api/hosts`: the same, as JSON.
- `POST /api/accounts` with `{"username", "password"}`: makes an account and signs the browser in
  (201); 409 for a taken username, 400 for one that cannot be used or a password under 12 characters.
- `POST /api/session` with `{"username", "password"}`: signs a browser in (204, with the session
  cookie); 401 for a wrong username or password. `DELETE /api/session` signs it out.
- `PUT /api/account/password` with `{"current", "password"}`: changes the password, signs every
  browser of the account out and this one in again (204).
- `POST /api/host-enrollments` with `{"host"}` (from a host, no account): 201 with `{"id",
  "code", "secret", "expiresAt"}`. `POST /api/host-enrollments/{id}/key?wait=50s` (at most 1m)
  with `Authorization: Bearer <secret>`: 200 `{"key", "host"}` once the code is entered (once only),
  202 while it is not, 404 once it expired.
- `POST /api/host-enrollments/claim` with `{"code"}`: adds the host showing that code to the
  account (200 `{"host"}`); 404 for no such code, 409 for a host name another account holds.
- `DELETE /api/hosts/{host}`: removes the host from the account, with everything it reported (204).

Writes from a browser (everything but the host's own enrollment calls) must come from this
origin (another site's request is refused with 403), and those with a body must send JSON (else
415), which another site cannot send without the server's leave; the cookie is SameSite=Strict too.
- `POST /api/dictation/token` with `{}`: a token for one AssemblyAI streaming session (60 seconds to
  open it, 30 minutes long), minted with the server's key, which the browser never sees: 200
  `{"token", "url", "expiresIn"}`; 503 with no `ASSEMBLYAI_API_KEY`, 502 with AssemblyAI's reason
  when it refuses. Nothing is stored.
- `POST /api/sql` with `{"query": "...", "limit": 1000}`, or `GET /api/sql?q=...&limit=`: one
  read-only query, as the account's own role, so it reads only the account's rows. Returns `{"columns": [{"name", "type"}], "rows": [[...]], "rowCount",
  "truncated"}`, or 400 with `{"error"}`. At most 10000 rows; 5 seconds per query.
- `GET /api/host/binary/{os}/{arch}`: the host binary at the server's version.
- `GET /dictation.js`: the dictation engine shared by the chat box, the new agent and workspace forms,
  and the workflow run dialog.
- `GET /ui.css`: the one stylesheet every page shares, with the top bar (`layout.html`) that links the
  pages.
- `GET /workflows`: a new task (for workflows triggered by a task: pick the workflow, which names
  its host, with the default first and selected), the account's workflows and their runs. Without
  workflows, a button adds the starting one (`workflow.Starting`) on the account's first host.
- Task workflow cards offer Make default (one per account) and Clear default; without a default,
  the task picker keeps creation order. The choice is readable as `workflows.is_default`.
- `PUT /api/workflows/{id}/default` with `{"default": true}` (or `false` to clear):
  chooses the account's default task workflow (204); 404 for another account's workflow.
- `GET /workflows/{id}`: the editor: the host and trigger, how the project is found, the orchestrator, and
  the steps (add, reorder, edit, remove), or the definition as JSON. Saving makes a new version.
  A project that is always the same repository is picked from those the workflow's host reported
  (its workspaces' `project_root`s), or typed. The “Run this workflow” dialog takes the run's task;
  with `ASSEMBLYAI_API_KEY` set, its mic button or ⌘D/Ctrl+D uses the shared dictation controls.
- `GET /workflow-runs/{id}`: a run: its project, the orchestrator and its sub-agents, each step's
  state and what happened in it, and, while it waits, the orchestrator's note with a reply box.
- `POST /api/workflows` with `{"name", "description", "definition"}`: 201 `{"id", "version": 1}`;
  400 for a definition that cannot run. `PUT /api/workflows/{id}`, the same body: the next version.
- `POST /api/workflow-runs` with `{"workflowId", "input"}`: starts a run on the workflow's host
  (201, with its `Location`); 400 for a workflow saved without one.
- `POST /api/workflow-runs/{id}/reply` with `{"text"}`: answers the note the run waits on (204;
  409 when it waits on none). `POST /api/workflow-runs/{id}/cancel` with `{}`: ends the run and
  stops its orchestrator (204).
- `POST /api/workflow-runs/{id}/moves` with `{"move", "directory", "step", "summary", "why",
  "note"}` and `Authorization: Bearer <the run's host's key>`: `intelligence workflow`. Answers
  `{"message"}`, what the agent reads next; 400 or 409 (with the reason as the message) for a move
  that cannot be made, 404 for a run not on that host.
- `GET /workflows/email`: the mailbox (connect, the account holder's own addresses, disconnect) and
  what each email did.
- `POST /api/gmail/sign-in` with `{}`: `{"url", "paste"}`, Google's consent page; `paste` when the
  client is a Desktop one, whose redirect to `http://localhost` does not load and is pasted back.
  503 when email triggers are off.
- `GET /gmail/callback`: where Google sends a web client's browser. The session cookie is
  SameSite=Strict, so it is not sent on that redirect: the page posts the code from this origin.
- `POST /api/gmail/connect` with `{"code", "state"}` or `{"url"}` (where Google sent the browser):
  connects the mailbox, `{"address"}`; 400 for a sign-in started by another account or expired.
- `PUT /api/gmail/owners` with `{"owners": [addresses]}` (204), and `DELETE /api/gmail` (204;
  its emails stay).
- `GET /healthz`: `{"status":"ok","version":"<commit>","database":"ok"}`, 503 when the database is
  unreachable.

Pages read through SQL and write through the API (see `AGENTS.md`).

## Configuration

Server:

- `DATABASE_URL`: the app's database, as a role that owns its tables and can create roles.
- `READONLY_DATABASE_URL`: the same database as the read-only role. The server creates or updates
  that role (name and password from the URL) and its grants on every start. Each account's role
  is named after it, and its password derived from its password (keep the name under 40 characters).
- `PORT`: listen port, default `8080`.
- `HOST_BINARIES`: where the host binaries are; default `/usr/local/share/intelligence/host`.
- `ASSEMBLYAI_API_KEY`: optional; turns on dictation in the chat box, new agent and workspace forms,
  and workflow run dialog. Unset, there is none.
- `GOOGLE_OAUTH_CLIENT_JSON` and `CONNECTIONS_KEY`: optional, together; turn on email triggers. The
  first is a Google OAuth client's JSON (raw or base64), Desktop ("installed") or web (with
  `<PUBLIC_URL>/gmail/callback` as a redirect URI), with the Gmail API enabled. The second is 32
  random bytes in base64 (`openssl rand -base64 32`); it seals each mailbox's token, so changing
  it means connecting again.
- `PUBLIC_URL`: optional; the server's origin, for Google's redirect. Default each request's.

The version is stamped at build (`-X main.buildCommit`, from Coolify's `SOURCE_COMMIT` build
argument); else `SOURCE_COMMIT` at run time, else Go's VCS stamp, else `dev`.

Host (in `~/.config/intelligence/host.env`):

- `HOST_SERVER`: the server's base URL.
- `HOST_KEY_FILE`: optional; where the host keeps its key; default `~/.config/intelligence/host.key`.
  With none there, the host joins an account (above).
- `HOST_NAME`: optional; default the lowercased hostname.
- `HOST_INTERVAL`: optional time between re-reads of the daemon's status (reported when it
  changes); default `15s`.
- `HOST_PASEO_HOME`: optional Paseo home; default `~/.paseo`.
- `HOST_AUTOUPDATE`: `off` keeps the installed binary; default on.
- `HOST_PROBE_SOCKET`: the eBPF probe's socket; default `/run/intelligence/probe.sock`; `off` for none.

Hosts were called dev boxes until migration `0011`. A host installed then runs the new binary when
it updates itself, from its old unit, and keeps working: `intelligence devbox` is `intelligence host`,
each `HOST_` variable may still be set by its `DEVBOX_` name (on the server, `DEVBOX_BINARIES` too),
`~/.config/intelligence/devbox.key` is used while there is no `host.key`, and the server still serves
updates at `/api/devbox/binary/{os}/{arch}` and sends `/devboxes/...` pages to `/hosts/...`.
Running `deploy/install-host` again moves the host to the new names (env file, key and unit).

Probe (in `/etc/intelligence/probe.env`, written by `deploy/install-probe`):

- `PROBE_SERVER`: the server's base URL, to follow its version.
- `PROBE_USER`: the host user, the only one who may read the socket.
- `PROBE_SOCKET`: optional; default `/run/intelligence/probe.sock`.
- `PROBE_AUTOUPDATE`: `off` keeps the installed binary; default on.

## Deploy

- Server: Coolify builds the `Dockerfile` (with the commit as `SOURCE_COMMIT`) and redeploys on
  every push to `main`. The app has no public domain: its port is bound to loopback on the machine it
  runs on, and `tailscale serve` publishes it over HTTPS to the tailnet only. That machine's details
  are kept outside this repo.
- Host: once per host, from a checkout of `main`, run `deploy/install-host` (the first run needs
  `HOST_SERVER` in the environment). It prints the host's code: enter it on the server's `/account`
  page. After that the host updates itself.
  Logs: `journalctl --user -u intelligence-host`.
- eBPF probe: once per Linux host, after the host client, run `deploy/install-probe` (it needs
  sudo). After that the probe updates itself. Logs: `journalctl -u intelligence-probe`.

## License

MIT. See `LICENSE`.
