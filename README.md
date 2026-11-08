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
  status, workspaces and agents, then follows the updates the daemon pushes. It POSTs a report to
  the server within a second of a change and every 15 seconds (which also re-reads the daemon's
  status). The box dials out, so it needs no inbound port, and the server never reaches into a box.
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
- `schema_migrations`.

Each report replaces its box's workspaces and agents. A box that cannot reach its daemon has none,
so nothing stale reads as current. The `devbox_status` view adds `online` (reported within the last
minute) and counts; `workspace_status` adds each workspace's agent counts.

### Transcripts

After each report the box syncs the transcripts of its loaded agents (running, idle, error) whose
activity changed: the whole timeline the first time, then only what follows the server's copy
(re-reading the last entry, which may still be growing), in uploads of at most 4 MB. It reads where
the server's copies end once at start, so a restart resumes. A new epoch (the daemon replaced the
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
subscribing), and pushed `workspace_update` and `agent_update` messages. The hello opts in to
explicit subscriptions; without that the daemon streams every running agent's timeline. It decodes
only the fields stored here: agent records can carry credentials in other fields. The protocol is
not published as stable; `internal/paseo/paseotest` is a fake daemon that validates requests the
way Paseo 0.15 does, and the client was checked against a live daemon.

### Self-update

Every response carries `X-Intelligence-Version`, the server's commit. The image builds the dev box
binary for linux and darwin (amd64 and arm64) at that commit and serves it at
`/api/devbox/binary/{os}/{arch}` with its sha256. When a dev box sees a version other than its own
after any call, it downloads that binary (at most once a minute), checks the sha256 and that the
binary runs and reports that version, replaces its own executable and re-executes itself. A box
follows the server both forward and back, so a rollback rolls the boxes back too.

## Develop

```sh
make test   # gofmt, go vet, go test -race against a throwaway Postgres in Docker
make run    # http://localhost:8080 (needs DATABASE_URL and READONLY_DATABASE_URL)
```

Tests talk to a fake Paseo daemon (`internal/paseo/paseotest`). Database tests get their own
database and read-only role (`internal/dbtest`) on the server in `TEST_DATABASE_URL`, which
`make test` sets when it is empty.

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
- `POST /api/devboxes/transcripts`, `GET /api/devboxes/{box}/transcripts`: a box's transcript uploads
  and where the server's copies end. Need the dev box token.
- `GET /api/devboxes`: the same, as JSON.
- `POST /api/sql` with `{"query": "...", "limit": 1000}`, or `GET /api/sql?q=...&limit=`: one
  read-only query. Returns `{"columns": [{"name", "type"}], "rows": [[...]], "rowCount",
  "truncated"}`, or 400 with `{"error"}`. At most 10000 rows; 5 seconds per query.
- `POST /api/devboxes/report`: a dev box's report. Needs `Authorization: Bearer <DEVBOX_TOKEN>`;
  refused with 503 while the server has no token.
- `GET /api/devbox/binary/{os}/{arch}`: the dev box binary at the server's version.
- `GET /healthz`: `{"status":"ok","version":"<commit>","database":"ok"}`, 503 when the database is
  unreachable.

The read routes are open to the tailnet.

## Configuration

Server:

- `DATABASE_URL`: the app's database, as a role that owns its tables and can create roles.
- `READONLY_DATABASE_URL`: the same database as the read-only role. The server creates or updates
  that role (name and password from the URL) and its grants on every start.
- `DEVBOX_TOKEN`: the token dev boxes report with.
- `PORT`: listen port, default `8080`.
- `DEVBOX_BINARIES`: where the dev box binaries are; default `/usr/local/share/intelligence/devbox`.

The version is stamped at build (`-X main.buildCommit`, from Coolify's `SOURCE_COMMIT` build
argument); else `SOURCE_COMMIT` at run time, else Go's VCS stamp, else `dev`.

Dev box (in `~/.config/intelligence/devbox.env`):

- `DEVBOX_SERVER`: the server's base URL.
- `DEVBOX_TOKEN`: the server's dev box token.
- `DEVBOX_NAME`: optional; default the lowercased hostname.
- `DEVBOX_INTERVAL`: optional; default `15s`.
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
