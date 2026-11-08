# Intelligence

A web app, inspired by agent-factory, that will orchestrate coding agents through Paseo on a fleet
of dev boxes. For now it shows each dev box's Paseo daemon, and all of its state is readable with
read-only SQL.

## How it fits together

One binary, three commands:

- `intelligence` (or `intelligence server`): the web app. It runs in Coolify with a Postgres
  database, reachable only on the tailnet.
- `intelligence devbox`: runs on each dev box as a systemd user service. Every 15 seconds it reads
  the local Paseo daemon (`paseo status --json`, `paseo provider ls --json`, `paseo ls --json`) and
  POSTs a report to the server. The box dials out, so it needs no inbound port, and the server never
  reaches into a box.
- `intelligence version`: prints the commit the binary was built from.

### State and the read-only SQL contract

All app state is in Postgres (`internal/db/migrations/`), in the `public` schema. `POST /api/sql`
runs a query as a separate login role that can only SELECT, so every piece of state can be read
and none written. Before each deploy the contract is checked (`db.CheckContract`): every relation is
in `public` and readable by that role, and the role cannot write. The image build runs every test
against a throwaway Postgres, so a broken contract fails the build and nothing deploys; the server
checks it again against production when it starts.

A box is online while it reported within the last minute (the `devbox_status` view).

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

Tests fake `paseo` with captured output in `internal/devbox/testdata/`. Database tests get their own
database and read-only role (`internal/dbtest`) on the server in `TEST_DATABASE_URL`, which
`make test` sets when it is empty.

## Routes

- `GET /`: dev boxes and their Paseo daemons. Refreshes every 15 seconds.
- `GET /api/devboxes`: the same, as JSON.
- `POST /api/sql` with `{"query": "...", "limit": 1000}`, or `GET /api/sql?q=...&limit=`: one
  read-only query. Returns `{"columns": [{"name", "type"}], "rows": [[...]], "rowCount",
  "truncated"}`, or 400 with `{"error"}`. At most 10000 rows; 5 seconds per query.
- `POST /api/devboxes/report`: a dev box's report. Needs `Authorization: Bearer <DEVBOX_TOKEN>`;
  refused with 503 while the server has no token.
- `GET /api/devbox/binary/{os}/{arch}`: the dev box binary at the server's version.
- `GET /healthz`: `{"status":"ok","version":"<commit>","database":"ok"}`, 503 when the database is
  unreachable.

The read routes are open to the tailnet. `tailscale serve` sets `Tailscale-User-Name` for requests
from people, which the home page shows; it is for display only, not for authorization.

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
- `DEVBOX_PASEO`: optional paseo executable; default `paseo` on PATH.
- `DEVBOX_AUTOUPDATE`: `off` keeps the installed binary; default on.

## Deploy

- Server: Coolify builds the `Dockerfile` (with the commit as `SOURCE_COMMIT`) and redeploys on
  every push to `main`. The app has no public domain: its port is bound to the host's loopback, and
  `tailscale serve` publishes it over HTTPS to the tailnet only. Host details are kept outside this
  repo.
- Dev box: once per box, from a checkout of `main`, run `deploy/install-devbox` (the first run needs
  `DEVBOX_SERVER` and `DEVBOX_TOKEN` in the environment). After that the box updates itself.
  Logs: `journalctl --user -u intelligence-devbox`.
