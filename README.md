# Intelligence

A web app, inspired by agent-factory, that will orchestrate coding agents through Paseo on a fleet
of dev boxes. For now it shows each dev box's Paseo daemon and how much of each subscription plan
(every Claude login, Codex, Muse, Antigravity) is used over time, and all of its state is readable
with read-only SQL. People sign in to accounts with a password, and each account sees only its own
dev boxes and everything they report.

## How it fits together

One binary, four commands:

- `intelligence` (or `intelligence server`): the web app. It runs in Coolify with a Postgres
  database, reachable only on the tailnet.
- `intelligence devbox`: runs on each dev box as a systemd user service. It keeps one connection to
  the local Paseo daemon over the daemon's own WebSocket protocol (`internal/paseo`), reads its
  status, workspaces and agents, then follows the updates the daemon pushes. It holds one
  WebSocket stream to the server (below) for its reports, its agents' transcripts, its plans' usage
  and the server's commands, each handled on its own (`devbox.Box`), so a slow transcript upload
  holds up no report. The box dials out, so it needs no inbound port, and the server never dials a box.
- `intelligence probe`: runs as root on each Linux dev box (a systemd system service): the eBPF
  probe (`internal/probe`) that charges every process's CPU to its Paseo agent and records OOM kills.
  The dev box client reads it over a Unix socket.
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
the dev boxes it holds keys for (`devbox_keys`), and with them their workspaces, agents,
transcripts, commands, resources, and the plan logins its boxes read. The page routes read through
the same role, so a page shows an account only what its SQL would.

Before each deploy the contract is checked (`db.CheckContract`): every relation is in `public` and
readable by the read-only role, which cannot write or bypass row-level security; every table but
`schema_migrations` has a row-level security policy; every view is `security_invoker`; and every
account's role is read-only and bound by row-level security. The image build runs every test
against a throwaway Postgres, so a broken contract fails the build and nothing deploys; the server
checks it again against production when it starts.

The hierarchy is devboxes > workspaces > agents:

- `devboxes`: each box's latest report: whether it is connected to its daemon (`connected`, else
  why in `paseo_error`), the daemon's version, listen address, pid, start and providers.
- `workspaces`: each box's Paseo workspaces: a directory, local checkout or git worktree that agents
  run in (`kind`), with its status, project and its repository's main checkout (`project_root`),
  branch, diff and directory. `listed = false` is a
  workspace Paseo no longer lists (archived or background) that still has an agent in it.
- `agents`: every unarchived agent, in its workspace (`box`, `workspace_id`), with its model,
  thinking level, status, attention, parent (for sub-agents) and times.
- `transcripts` and `transcript_entries`: each agent's transcript, its projected Paseo timeline.
  An entry keeps the daemon's item whole (`item`, jsonb); `kind` (user_message, assistant_message,
  reasoning, tool_call, ...), `tool`, `status` and `text` are generated from it. NUL characters,
  which Postgres cannot store, become U+FFFD.
- `commands`: what the server asked a box to do (below): its kind, args, status, result or error,
  and when it was created, sent, started, finished and when it expires.
- `host_minutes`, `agent_minutes`, `oom_kills` and `host_probes`: each box's resources over time
  (below), with the views `host_current`, `host_days` and `agent_days`.
- `usage_accounts`, `usage_readings`, `usage_active` and `usage_sources`: subscription plans' usage
  over time (below), with the views `usage_current`, `usage_pace`, `usage_cycles` and `usage_totals`.
- `accounts` (username, argon2id password hash, its role), `sessions` (signed-in browsers; a hash
  of each cookie's token), `devbox_keys` (each box's account and a hash of its key) and
  `devbox_enrollments` (boxes waiting for their code to be entered). An account reads only its own.
- `schema_migrations`.

Each report replaces its box's workspaces and agents. A box that cannot reach its daemon has none,
so nothing stale reads as current. The `devbox_status` view adds `online` (the box holds a stream
to the server, `stream_id`, which was seen within the last minute) and counts; `workspace_status`
adds each workspace's agent counts.

### The stream

A dev box dials `GET /api/devboxes/{box}/stream` (a WebSocket, with its own key) and keeps it
open, dialing again 5 seconds after it drops. Each frame is a JSON `devbox.Frame`:

- server to box: `hello` first (where its transcript copies end), then `command`s. The server's
  version is in the dial's `X-Intelligence-Version` header.
- box to server: `report` (on each new stream, and within a second of a change; the box re-reads
  the daemon's status every 15 seconds and reports it if it changed), `transcript` batches, `usage`
  (every 5 minutes), `host` (every minute) and `command_update`s. Each carries an `id`, and the server answers each with an
  `ack` once it is stored (or with why not), transcript batches on their own so a large one holds up
  nothing else.

Each side pings the other: the box every 30 seconds (an unanswered ping, or a frame not acked in 30
seconds, drops the stream, and the box dials again) and the server every 20 seconds, recording the
box as seen. A newer stream for the same box takes it over and ends the older one, on whichever
server instance holds it.

Server instances learn of each other's changes through Postgres `NOTIFY` on the `fleet` channel
(`fleet.Changes`): a queued command, a command's update and a stream taking a box over. Nothing on
the server polls.

### Accounts and dev box keys

An account is a username (lowercase letters, digits, `-` and `_`) and a password of at least 12
characters, hashed with argon2id. Anyone who can reach the server can make one; an account starts
with no dev boxes and sees nothing until one joins it. A signed-in browser holds a random session
token in a cookie (HttpOnly, Secure, SameSite=Strict, 30 days); Postgres keeps only its hash.
Signing out ends that session; changing the password ends all of them.

There are no shared tokens. A dev box joins an account with a code:

1. A box with no key (`~/.config/intelligence/devbox.key`) asks to join
   (`POST /api/devbox-enrollments`), and logs a code such as `7KQ2-M9XD`, good for an hour.
2. Someone signed in enters the code on `/account` (`POST /api/devbox-enrollments/claim`). A box
   name another account holds cannot be claimed.
3. The box, waiting on `POST /api/devbox-enrollments/{id}/key` with the secret only it holds, gets
   its key at once (woken by the claim, not by polling), keeps it (mode 600) and streams with it.

A key is good for its own box name only. Removing a box from its account (`/account`) deletes its
key and everything it reported; the box's next dial is refused, so it forgets the key and asks to
join again with a new code. A box that expires a code without its being entered asks for a new one.

### Commands

The server tells a box what to do through its stream. A command is queued in `commands`
(`POST /api/devboxes/{box}/commands`, below), goes down the box's stream at once if it is connected
(to any server instance; else when it next connects), and the box runs it
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
- `archive_workspace`: `workspaceId`. A Paseo-made worktree is removed with it.
- `rename_agent`: `agentId`, `title`. `cancel_agent` (stops its current turn) and `archive_agent`:
  `agentId`.
- `send_message`: `agentId`, `text`, and optionally `behavior`, which says what happens to a turn
  the agent is running: `steer` (the default) adds the message to it (Paseo interrupts instead
  when the provider cannot steer), `interrupt` stops it and starts a new one, and `queue` waits
  for it to end. Paseo has no queue of its own, so the box holds queued messages: one stays
  `running` until its agent is not running and every message queued for that agent before it
  (by creation time) has gone, then starts a turn. It waits until the command expires, so give it
  a longer `ttl`; one whose agent is archived meanwhile fails. On an agent that is not running,
  all three start a turn.

Delivery is at least once: a command sent down a stream that dropped before its result arrived is
sent again when the box reconnects. Each kind is safe to repeat. Creations and messages carry the
command id to Paseo as their idempotency key, renames and archives are idempotent, and a box
remembers its last 1000 results, so a repeat gets the first result again without running twice. A
command not finished by its expiry (10 minutes by default) is `expired`: never delivered, or
delivered with no result (it may have run). A box does not start an expired command, and a result
that arrives after expiry replaces it.

### Plan usage

Every 5 minutes each box reads every provider's own meter (`internal/usage`) and sends it up its
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

A tool missing from a box is skipped; a failed read is recorded in `usage_sources` and stores no
reading. The same login read from several boxes is one login. Windows are `7d`, `5h` and pools'
(`fable/7d`); reset times are rounded to the minute, as providers jitter them.

`usage_readings` keeps the history as runs: a row is one window holding one value from
`first_observed` to `last_observed`, and a new row starts only when the value or reset changes. A
reading older than the newest is ignored (another box's older cswap cache), and a window still at
0% whose reset keeps moving (not started) stays one run. `usage_active` records which login each
box's tool used, and when (cswap switches Claude logins).

The views: `usage_current` (each window's latest value; `remaining_pct` is null once the window
has reset with no reading since, and `stale` after 15 minutes without one), `usage_pace` (% left
minus % of the window still to run: positive is under-spending), `usage_cycles` (the most of each
window used before each reset, so how much of each week each login used) and `usage_totals`
(logins, and weeks of quota left across them, per provider).

### Host resources

Every box measures itself (`internal/host`) and sends each finished minute up its stream as a
`host` frame; minutes the server could not take are kept (up to three hours) and sent with the next.

- The host, every 5 seconds: CPU (busy %, from `/proc/stat`), load, memory and swap
  (`/proc/meminfo`), the home filesystem's space, pressure (`/proc/pressure`: the share of time
  tasks waited for CPU, memory or I/O), the kernel's OOM kill count (`/proc/vmstat`) and the Paseo
  daemon's cgroup (all its agents: memory less page cache, its peak, its OOM kills). A minute keeps
  the average and peak CPU, the least memory and disk available, the most pressure, and the kills.
  macOS boxes report CPU, load, memory, swap and disk (`sysctl`, `vm_stat`, `iostat`).
- Each agent, every 15 seconds: its processes are those whose environment names it
  (`PASEO_AGENT_ID`, which commands inherit); a minute keeps their most memory (proportional set
  size, so shared pages count once) and how many there were.
- Each agent's CPU and commands, and every OOM kill, from the eBPF probe where it runs (below);
  otherwise CPU comes from the processes alive at each 15-second walk, which misses short-lived
  commands (`cpu_source` says which).

The server keeps 90 days of minutes (OOM kills are kept). `host_current` is each box's newest
minute; `host_days` and `agent_days` sum days.

### The eBPF probe

`intelligence probe` runs as root on each Linux box (`deploy/install-probe`). It loads four small
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
`/run/intelligence/probe.sock`, readable by the dev box user only; it holds no token. Checked on a
dev box: an agent's 200 short commands and CPU burn were charged within 1% of the kernel's own
accounting, and an OOM kill in a 64 MB scope was recorded with its agent.

The probe follows the server's version like the dev box client: every minute it asks the server's
version and, when it differs, downloads the server's binary, checks its sha256, checks that it runs
the probe (`<binary> probe version`, so a server rolled back to before the probe never replaces
it), replaces `/usr/local/lib/intelligence/intelligence` and re-executes. Its trust is the dev box
client's: the client already runs the server's binary as the dev box user. The unit limits it to
the capabilities it needs (BPF, perfmon, ptrace read of other processes' environment, memlock).

### Transcripts

After each report, and every 2 seconds while an agent is running, the box syncs the transcripts of
its loaded agents (running, idle, error) whose activity changed: the whole timeline the first time,
then only what follows the server's copy (re-reading the last entry, which may still be growing), in
batches of at most 4 MB over the stream. It takes where the server's copies end from each stream's
hello, so a restart or a reconnect resumes. A new epoch (the daemon replaced the
timeline) replaces the copy.

**A closed agent's transcript is never read:** the daemon answers a timeline read by loading the
agent, which starts a closed agent again. An agent's transcript is copied while it is loaded and
kept after Paseo closes it; one closed before syncing reached it has none until it is opened again.
A transcript goes when its agent is gone from a connected box's report (archived); a box that loses
its daemon keeps its transcripts.

### Talking to Paseo

`internal/paseo` speaks the daemon's protocol directly: a WebSocket at `ws://<listen>/ws` from
`~/.paseo/paseo.pid`, a hello authenticated with `~/.paseo/local-credential`, then
`daemon.get_status`, `fetch_workspaces` and `fetch_agents` requests (paged, the first page
subscribing), and pushed `workspace_update` and `agent_update` messages. Commands use
`workspace.create`, `agent.create`, `workspace.title.set`, `archive_workspace`, `update_agent`,
`send_agent_message`, `cancel_agent` and `archive_agent`; a refusal comes back as the daemon's
error. The hello opts in to
explicit subscriptions; without that the daemon streams every running agent's timeline. It decodes
only the fields stored here: agent records can carry credentials in other fields. The protocol is
not published as stable; `internal/paseo/paseotest` is a fake daemon that validates requests the
way Paseo 0.15 does, and the client was checked against a live daemon.

### Self-update

Every response carries `X-Intelligence-Version`, the server's commit. The image builds the dev box
binary for linux and darwin (amd64 and arm64) at that commit and serves it at
`/api/devbox/binary/{os}/{arch}` with its sha256. When a dev box sees a version other than its own
as it dials its stream (a refused dial too, so a box can update to a server it cannot talk to), it
downloads that binary (at most once a minute), checks the sha256 and that the binary runs and
reports that version, replaces its own executable and re-executes itself. A deploy restarts the
server, which drops every stream, so each box sees the new version within seconds. A box
follows the server both forward and back, so a rollback rolls the boxes back too.

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

Every page and read route needs a signed-in account and shows only its own boxes: a page sends a
browser that is not signed in to `/login`, and an API route answers 401. Only `/healthz`, `/login`,
the account and enrollment routes, the dev box binaries and the streams (with a box key) are open.

- `GET /`: a collapsible tree of hosts > workspaces (with their project) > agents (with their status),
  with links to make a workspace on each host and start an agent in each workspace.
  Hosts start open, and so do workspaces with an agent running or needing attention; workspaces without
  agents are folded under a count. Problems (Paseo not connected, a client not on the server's version)
  show under the host. The tree refreshes in place every 15 seconds and keeps what was opened or closed.
  Above it, Plans: every login with each window's % left, its reset, pace and the boxes using it.
- `GET /usage?days=7` (1 to 365): every login with a chart of each window's used % over the days,
  and its weekly peaks.
- `GET /api/usage`: the logins and their windows, as JSON.
- `GET /hosts?hours=24` (1 to 2160): each box's CPU and memory charts, its OOM kills and its agents
  ranked by CPU, with their most memory and commands started. The home page shows each box's CPU,
  memory, disk and pressure under its name, and each agent's memory beside it.
- `GET /api/hosts`: each box's newest minute, probe and agents' memory, as JSON.
- `GET /devboxes/{box}/workspaces/{ws}/agents/new`: the form that starts an agent in a workspace.
  `GET /devboxes/{box}/workspaces/new`: the form that makes a workspace with an agent in it, by
  default a new git worktree of one of the box's repositories (its workspaces' `project_root`s), or
  of a directory typed in. The home page links to both. Each asks what the agent should do, an
  optional title, and the provider (those the box reports available), model, thinking and mode,
  which start as Opus 5.5, medium thinking, `bypassPermissions`. The page sends a command to the
  commands API, follows it through `/api/sql`, then opens the new agent's transcript; a failure
  shows Paseo's reason.
- `GET /login`: signs a browser in, or makes an account, through the account APIs.
- `GET /account`: the account's dev boxes (online, when each joined and last used its key), how to
  install one, the form that adds a box by its code, and the password form.
- `GET /devboxes/{box}/agents/{id}`: one agent's transcript as turns: messages rendered as Markdown
  (raw HTML shown as text, unsafe links dropped, images named rather than loaded), thinking folded to
  a line, and runs of tool calls as a compact list that opens per call (commands with exit codes,
  diffs, reads, writes, searches). Long output is cut, with a link to the whole entry. Search,
  thinking and tool toggles, and the other agents in the workspace. While the agent is loaded the
  page re-fetches from its last block every 4 seconds and replaces it, so a growing message updates
  in place. A chat box at the bottom sends the agent a message through the commands API: Enter
  (or Steer) steers its running turn, Queue (Alt+Enter) waits for the turn to end, and Interrupt
  (Cmd/Ctrl+Enter) stops it; Shift+Enter is a new line. Messages not yet delivered are listed
  above it, read through `/api/sql`; one that fails says why and comes back into the box.
  Under its title, the agent's resources from `agent_minutes` and `oom_kills`: its CPU time (average
  and peak cores), memory now and at its peak, most processes, commands started and OOM kills, opening to charts of its CPU and memory over its life; re-read every minute while live.
- `GET /devboxes/{box}/agents/{id}/resources`: those resources alone, as HTML, for the live page.
- `GET /devboxes/{box}/agents/{id}/blocks?from={seq}`: the blocks from a seq on, as HTML, for the live
  page (`X-Epoch`, `X-Agent-Status`).
- `GET /devboxes/{box}/agents/{id}/entries/{seq}`: one entry whole.
- `GET /api/devboxes/{box}/stream`: a dev box's stream (WebSocket). Needs `Authorization: Bearer
  <the box's key>`, a key for that box name; else 401.
- `POST /api/devboxes/{box}/commands` with `{"kind": "...", "args": {...}, "ttl": "10m"}`: queue a
  command (kinds above; `ttl` from 1s to 24h) for one of the account's boxes. 202 with the command
  and its `Location`; 400 for a kind or args that do not parse, 404 for a box that is not the
  account's.
- `GET /api/devboxes/{box}/commands/{id}`, optionally `?wait=30s` (at most 1m): the command, after
  waiting until it finishes or the wait is up (woken by its update, not polling). 404 for another
  account's box.
- `GET /api/devboxes`: the same, as JSON.
- `POST /api/accounts` with `{"username", "password"}`: makes an account and signs the browser in
  (201); 409 for a taken username, 400 for one that cannot be used or a password under 12 characters.
- `POST /api/session` with `{"username", "password"}`: signs a browser in (204, with the session
  cookie); 401 for a wrong username or password. `DELETE /api/session` signs it out.
- `PUT /api/account/password` with `{"current", "password"}`: changes the password, signs every
  browser of the account out and this one in again (204).
- `POST /api/devbox-enrollments` with `{"box"}` (from a dev box, no account): 201 with `{"id",
  "code", "secret", "expiresAt"}`. `POST /api/devbox-enrollments/{id}/key?wait=50s` (at most 1m)
  with `Authorization: Bearer <secret>`: 200 `{"key", "box"}` once the code is entered (once only),
  202 while it is not, 404 once it expired.
- `POST /api/devbox-enrollments/claim` with `{"code"}`: adds the box showing that code to the
  account (200 `{"box"}`); 404 for no such code, 409 for a box name another account holds.
- `DELETE /api/devboxes/{box}`: removes the box from the account, with everything it reported (204).

Writes from a browser (everything but the dev box's own enrollment calls) must come from this
origin (another site's request is refused with 403), and those with a body must send JSON (else
415), which another site cannot send without the server's leave; the cookie is SameSite=Strict too.
- `POST /api/sql` with `{"query": "...", "limit": 1000}`, or `GET /api/sql?q=...&limit=`: one
  read-only query, as the account's own role, so it reads only the account's rows. Returns `{"columns": [{"name", "type"}], "rows": [[...]], "rowCount",
  "truncated"}`, or 400 with `{"error"}`. At most 10000 rows; 5 seconds per query.
- `GET /api/devbox/binary/{os}/{arch}`: the dev box binary at the server's version.
- `GET /ui.css`: the one stylesheet every page shares, with the top bar (`layout.html`) that links the
  pages.
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
- `DEVBOX_BINARIES`: where the dev box binaries are; default `/usr/local/share/intelligence/devbox`.

The version is stamped at build (`-X main.buildCommit`, from Coolify's `SOURCE_COMMIT` build
argument); else `SOURCE_COMMIT` at run time, else Go's VCS stamp, else `dev`.

Dev box (in `~/.config/intelligence/devbox.env`):

- `DEVBOX_SERVER`: the server's base URL.
- `DEVBOX_KEY_FILE`: optional; where the box keeps its key; default `~/.config/intelligence/devbox.key`.
  With none there, the box joins an account (above).
- `DEVBOX_NAME`: optional; default the lowercased hostname.
- `DEVBOX_INTERVAL`: optional time between re-reads of the daemon's status (reported when it
  changes); default `15s`.
- `DEVBOX_PASEO_HOME`: optional Paseo home; default `~/.paseo`.
- `DEVBOX_AUTOUPDATE`: `off` keeps the installed binary; default on.
- `DEVBOX_PROBE_SOCKET`: the eBPF probe's socket; default `/run/intelligence/probe.sock`; `off` for none.

Probe (in `/etc/intelligence/probe.env`, written by `deploy/install-probe`):

- `PROBE_SERVER`: the server's base URL, to follow its version.
- `PROBE_USER`: the dev box user, the only one who may read the socket.
- `PROBE_SOCKET`: optional; default `/run/intelligence/probe.sock`.
- `PROBE_AUTOUPDATE`: `off` keeps the installed binary; default on.

## Deploy

- Server: Coolify builds the `Dockerfile` (with the commit as `SOURCE_COMMIT`) and redeploys on
  every push to `main`. The app has no public domain: its port is bound to the host's loopback, and
  `tailscale serve` publishes it over HTTPS to the tailnet only. Host details are kept outside this
  repo.
- Dev box: once per box, from a checkout of `main`, run `deploy/install-devbox` (the first run needs
  `DEVBOX_SERVER` in the environment). It prints the box's code: enter it on the server's `/account`
  page. After that the box updates itself.
  Logs: `journalctl --user -u intelligence-devbox`.
- eBPF probe: once per Linux box, after the dev box client, run `deploy/install-probe` (it needs
  sudo). After that the probe updates itself. Logs: `journalctl -u intelligence-probe`.

## License

MIT. See `LICENSE`.
