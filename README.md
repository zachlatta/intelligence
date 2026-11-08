# Intelligence

A web app, inspired by agent-factory, that will orchestrate coding agents through Paseo on a fleet
of dev boxes. For now it shows each dev box's Paseo daemon, and all of its state is readable with
read-only SQL.

## How it fits together

One binary, three commands:

- `intelligence` (or `intelligence server`): the web app. It runs in Coolify with a Postgres
  database, reachable only on the tailnet.
- `intelligence devbox`: runs on each dev box as a systemd user service. It keeps one connection to
  the local Paseo daemon over the daemon's own WebSocket protocol (`internal/paseo`), reads its
  status, workspaces and agents, then follows the updates the daemon pushes. It holds one
  WebSocket stream to the server (below) for its reports, its agents' transcripts and the server's
  commands. The box dials out, so it needs no inbound port, and the server never dials a box.
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
  run in (`kind`), with its status, project, branch, diff and directory. `listed = false` is a
  workspace Paseo no longer lists (archived or background) that still has an agent in it.
- `agents`: every unarchived agent, in its workspace (`box`, `workspace_id`), with its model,
  thinking level, status, attention, parent (for sub-agents) and times.
- `transcripts` and `transcript_entries`: each agent's transcript, its projected Paseo timeline.
  An entry keeps the daemon's item whole (`item`, jsonb); `kind` (user_message, assistant_message,
  reasoning, tool_call, ...), `tool`, `status` and `text` are generated from it. NUL characters,
  which Postgres cannot store, become U+FFFD.
- `commands`: what the server asked a box to do (below): its kind, args, status, result or error,
  and when it was created, sent, started, finished and when it expires.
- `schema_migrations`.

Each report replaces its box's workspaces and agents. A box that cannot reach its daemon has none,
so nothing stale reads as current. The `devbox_status` view adds `online` (reported within the last
minute) and counts; `workspace_status` adds each workspace's agent counts.

### The stream

A dev box dials `GET /api/devboxes/{box}/stream` (a WebSocket, with the dev box token) and keeps it
open, dialing again 5 seconds after it drops. Each frame is a JSON `devbox.Frame`:

- server to box: `hello` first (the server's version and where its transcript copies end), then
  `command`s.
- box to server: `report` (within a second of a change and every 15 seconds, which also re-reads
  the daemon's status), `transcript` batches and `command_update`s. Each carries an `id`, and the
  server answers each with an `ack` once it is stored (or with why not). A box frame not acked in
  30 seconds drops the stream, so a box notices a dead server on its next report; the server
  pings every 30 seconds, so it notices a dead box.

A newer stream for the same box replaces the older one.

### Commands

The server tells a box what to do through its stream. A command is queued in `commands`
(`POST /api/devboxes/{box}/commands`, below), goes down the box's stream at once if it is connected
to this server instance (else within 5 seconds, or when the box next connects), and the box runs it
against its daemon and reports `running`, then `succeeded` with a result (`{"workspaceId",
"agentId"}`) or `failed` with the daemon's reason. Kinds and their args:

- `create_workspace`: `cwd`, and optionally `title`; with `worktree: true`, a new git worktree of the
  repository at `cwd`, on `branch` from `base`.
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

- `GET /`: a collapsible tree of hosts > workspaces (with their project) > agents (with their status).
  Hosts start open, and so do workspaces with an agent running or needing attention; workspaces without
  agents are folded under a count. Problems (Paseo not connected, a client not on the server's version)
  show under the host. The tree refreshes in place every 15 seconds and keeps what was opened or closed.
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
  command (kinds above; `ttl` from 1s to 24h). Needs `Authorization: Bearer <OPERATOR_TOKEN>`;
  refused with 503 while the server has none. 202 with the command and its `Location`; 400 for a
  kind or args that do not parse, 404 for a box that has never reported.
- `GET /api/devboxes/{box}/commands/{id}`, optionally `?wait=30s` (at most 1m): the command, after
  waiting until it finishes or the wait is up.
- `GET /api/devboxes`: the same, as JSON.
- `POST /api/sql` with `{"query": "...", "limit": 1000}`, or `GET /api/sql?q=...&limit=`: one
  read-only query. Returns `{"columns": [{"name", "type"}], "rows": [[...]], "rowCount",
  "truncated"}`, or 400 with `{"error"}`. At most 10000 rows; 5 seconds per query.
- `GET /api/devbox/binary/{os}/{arch}`: the dev box binary at the server's version.
- `GET /healthz`: `{"status":"ok","version":"<commit>","database":"ok"}`, 503 when the database is
  unreachable.

The read routes are open to the tailnet. Changing anything on a box needs the operator token.

## Configuration

Server:

- `DATABASE_URL`: the app's database, as a role that owns its tables and can create roles.
- `READONLY_DATABASE_URL`: the same database as the read-only role. The server creates or updates
  that role (name and password from the URL) and its grants on every start.
- `DEVBOX_TOKEN`: the token dev boxes stream with.
- `OPERATOR_TOKEN`: the token that may send dev boxes commands. Unset, commands are refused.
- `PORT`: listen port, default `8080`.
- `DEVBOX_BINARIES`: where the dev box binaries are; default `/usr/local/share/intelligence/devbox`.

The version is stamped at build (`-X main.buildCommit`, from Coolify's `SOURCE_COMMIT` build
argument); else `SOURCE_COMMIT` at run time, else Go's VCS stamp, else `dev`.

Dev box (in `~/.config/intelligence/devbox.env`):

- `DEVBOX_SERVER`: the server's base URL.
- `DEVBOX_TOKEN`: the server's dev box token.
- `DEVBOX_NAME`: optional; default the lowercased hostname.
- `DEVBOX_INTERVAL`: optional time between reports; default `15s`.
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
