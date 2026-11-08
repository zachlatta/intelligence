# Intelligence

A web app, inspired by agent-factory, that will orchestrate coding agents through Paseo on a fleet
of dev boxes. For now it shows each dev box's Paseo daemon and how much of each subscription plan
(every Claude login, Codex, Muse, Antigravity) is used over time, and all of its state is readable
with read-only SQL.

## How it fits together

One binary, three commands:

- `intelligence` (or `intelligence server`): the web app. It runs in Coolify with a Postgres
  database, reachable only on the tailnet.
- `intelligence devbox`: runs on each dev box as a systemd user service. It keeps one connection to
  the local Paseo daemon over the daemon's own WebSocket protocol (`internal/paseo`), reads its
  status, workspaces and agents, then follows the updates the daemon pushes. It holds one
  WebSocket stream to the server (below) for its reports, its agents' transcripts, its plans' usage
  and the server's commands, each handled on its own (`devbox.Box`), so a slow transcript upload
  holds up no report. The box dials out, so it needs no inbound port, and the server never dials a box.
- `intelligence version`: prints the commit the binary was built from.

### State and the read-only SQL contract

All app state is in Postgres (`internal/db/migrations/`), in the `public` schema. `POST /api/sql`
runs a query as a separate login role that can only SELECT, so every piece of state can be read
and none written. Before each deploy the contract is checked (`db.CheckContract`): every relation is
in `public` and readable by that role, and the role cannot write. The image build runs every test
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
- `usage_accounts`, `usage_readings`, `usage_active` and `usage_sources`: subscription plans' usage
  over time (below), with the views `usage_current`, `usage_pace`, `usage_cycles` and `usage_totals`.
- `schema_migrations`.

Each report replaces its box's workspaces and agents. A box that cannot reach its daemon has none,
so nothing stale reads as current. The `devbox_status` view adds `online` (the box holds a stream
to the server, `stream_id`, which was seen within the last minute) and counts; `workspace_status`
adds each workspace's agent counts.

### The stream

A dev box dials `GET /api/devboxes/{box}/stream` (a WebSocket, with the dev box token) and keeps it
open, dialing again 5 seconds after it drops. Each frame is a JSON `devbox.Frame`:

- server to box: `hello` first (where its transcript copies end), then `command`s. The server's
  version is in the dial's `X-Intelligence-Version` header.
- box to server: `report` (on each new stream, and within a second of a change; the box re-reads
  the daemon's status every 15 seconds and reports it if it changed), `transcript` batches, `usage`
  (every 5 minutes) and `command_update`s. Each carries an `id`, and the server answers each with an
  `ack` once it is stored (or with why not), transcript batches on their own so a large one holds up
  nothing else.

Each side pings the other: the box every 30 seconds (an unanswered ping, or a frame not acked in 30
seconds, drops the stream, and the box dials again) and the server every 20 seconds, recording the
box as seen. A newer stream for the same box takes it over and ends the older one, on whichever
server instance holds it.

Server instances learn of each other's changes through Postgres `NOTIFY` on the `fleet` channel
(`fleet.Changes`): a queued command, a command's update and a stream taking a box over. Nothing on
the server polls.

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
- `rename_agent`: `agentId`, `title`. `send_message`: `agentId`, `text`. `cancel_agent` (stops its
  current turn) and `archive_agent`: `agentId`.

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

- `GET /`: a collapsible tree of hosts > workspaces (with their project) > agents (with their status),
  with links to make a workspace on each host and start an agent in each workspace.
  Hosts start open, and so do workspaces with an agent running or needing attention; workspaces without
  agents are folded under a count. Problems (Paseo not connected, a client not on the server's version)
  show under the host. The tree refreshes in place every 15 seconds and keeps what was opened or closed.
  Above it, Plans: every login with each window's % left, its reset, pace and the boxes using it.
- `GET /usage?days=7` (1 to 365): every login with a chart of each window's used % over the days,
  and its weekly peaks.
- `GET /api/usage`: the logins and their windows, as JSON.
- `GET /devboxes/{box}/workspaces/{ws}/agents/new`: the form that starts an agent in a workspace.
  `GET /devboxes/{box}/workspaces/new`: the form that makes a workspace with an agent in it, by
  default a new git worktree of one of the box's repositories (its workspaces' `project_root`s), or
  of a directory typed in. The home page links to both. Each asks what the agent should do, an
  optional title, and the provider (those the box reports available), model, thinking and mode,
  which start as Opus 5.5, medium thinking, `bypassPermissions`. The page sends a command to the
  commands API, follows it through `/api/sql`, then opens the new agent's transcript; a failure
  shows Paseo's reason. Both need a signed-in browser and send one to `/login` first.
- `GET /login`: signs a browser in through the session API.
- `GET /devboxes/{box}/agents/{id}`: one agent's transcript as turns: messages rendered as Markdown
  (raw HTML shown as text, unsafe links dropped, images named rather than loaded), thinking folded to
  a line, and runs of tool calls as a compact list that opens per call (commands with exit codes,
  diffs, reads, writes, searches). Long output is cut, with a link to the whole entry. Search,
  thinking and tool toggles, and the other agents in the workspace. While the agent is loaded the
  page re-fetches from its last block every 4 seconds and replaces it, so a growing message updates
  in place.
- `GET /devboxes/{box}/agents/{id}/blocks?from={seq}`: the blocks from a seq on, as HTML, for the live
  page (`X-Epoch`, `X-Agent-Status`).
- `GET /devboxes/{box}/agents/{id}/entries/{seq}`: one entry whole.
- `GET /api/devboxes/{box}/stream`: a dev box's stream (WebSocket). Needs `Authorization: Bearer
  <DEVBOX_TOKEN>`; refused with 503 while the server has no token.
- `POST /api/devboxes/{box}/commands` with `{"kind": "...", "args": {...}, "ttl": "10m"}`: queue a
  command (kinds above; `ttl` from 1s to 24h). Needs `Authorization: Bearer <OPERATOR_TOKEN>`, or a
  signed-in browser sending JSON from this origin (another site's request is refused with 403, a
  non-JSON one with 415); refused with 503 while the server has no token. 202 with the command and
  its `Location`; 400 for a kind or args that do not parse, 404 for a box that has never connected.
- `GET /api/devboxes/{box}/commands/{id}`, optionally `?wait=30s` (at most 1m): the command, after
  waiting until it finishes or the wait is up (woken by its update, not polling).
- `GET /api/devboxes`: the same, as JSON.
- `POST /api/session` with `{"token"}`: signs a browser in with the operator token (204), as a
  cookie holding a hash of the token (HttpOnly, Secure, SameSite=Strict, 30 days), so changing the
  token signs every browser out. `DELETE /api/session` signs it out. Requests from another site are
  refused.
- `POST /api/sql` with `{"query": "...", "limit": 1000}`, or `GET /api/sql?q=...&limit=`: one
  read-only query. Returns `{"columns": [{"name", "type"}], "rows": [[...]], "rowCount",
  "truncated"}`, or 400 with `{"error"}`. At most 10000 rows; 5 seconds per query.
- `GET /api/devbox/binary/{os}/{arch}`: the dev box binary at the server's version.
- `GET /healthz`: `{"status":"ok","version":"<commit>","database":"ok"}`, 503 when the database is
  unreachable.

The read routes are open to the tailnet. Changing anything on a box needs the operator token, or a
browser signed in with it. Pages read through SQL and write through the API (see `AGENTS.md`).

## Configuration

Server:

- `DATABASE_URL`: the app's database, as a role that owns its tables and can create roles.
- `READONLY_DATABASE_URL`: the same database as the read-only role. The server creates or updates
  that role (name and password from the URL) and its grants on every start.
- `DEVBOX_TOKEN`: the token dev boxes stream with.
- `OPERATOR_TOKEN`: the token that may send dev boxes commands, and signs a browser in. Unset,
  commands and sign-in are refused.
- `PORT`: listen port, default `8080`.
- `DEVBOX_BINARIES`: where the dev box binaries are; default `/usr/local/share/intelligence/devbox`.

The version is stamped at build (`-X main.buildCommit`, from Coolify's `SOURCE_COMMIT` build
argument); else `SOURCE_COMMIT` at run time, else Go's VCS stamp, else `dev`.

Dev box (in `~/.config/intelligence/devbox.env`):

- `DEVBOX_SERVER`: the server's base URL.
- `DEVBOX_TOKEN`: the server's dev box token.
- `DEVBOX_NAME`: optional; default the lowercased hostname.
- `DEVBOX_INTERVAL`: optional time between re-reads of the daemon's status (reported when it
  changes); default `15s`.
- `DEVBOX_PASEO_HOME`: optional Paseo home; default `~/.paseo`.
- `DEVBOX_AUTOUPDATE`: `off` keeps the installed binary; default on.

## Deploy

- Server: Coolify builds the `Dockerfile` (with the commit as `SOURCE_COMMIT`) and redeploys on
  every push to `main`. The app has no public domain: its port is bound to the host's loopback, and
  `tailscale serve` publishes it over HTTPS to the tailnet only. Host details are kept outside this
  repo.
- Dev box: once per box, from a checkout of `main`, run `deploy/install-devbox` (the first run needs
  `DEVBOX_SERVER` and `DEVBOX_TOKEN` in the environment). After that the box updates itself.
  Logs: `journalctl --user -u intelligence-devbox`.

## License

MIT. See `LICENSE`.
