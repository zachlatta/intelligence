# Intelligence

A web app, inspired by agent-factory, that will orchestrate coding agents through Paseo on a fleet
of dev boxes. For now it shows each dev box's Paseo daemon.

## How it fits together

One binary, two roles:

- `intelligence` (or `intelligence server`): the web app. It runs in Coolify, reachable only on the
  tailnet.
- `intelligence devbox`: runs on each dev box as a systemd user service. Every 15 seconds it reads
  the local Paseo daemon (`paseo status --json`, `paseo provider ls --json`, `paseo ls --json`) and
  POSTs a report to the server. The box dials out, so it needs no inbound port, and the server never
  reaches into a box.

The server keeps the latest report per box in memory: after a server restart, each box reappears
with its next report. A box that has not reported for a minute shows offline.

## Develop

```sh
make test   # gofmt, go vet, go test -race: must pass before every commit
make run    # http://localhost:8080
```

Tests fake `paseo` with captured output in `internal/devbox/testdata/`.

## Routes

- `GET /`: dev boxes and their Paseo daemons. Refreshes every 15 seconds.
- `GET /api/devboxes`: the same, as JSON.
- `POST /api/devboxes/report`: a dev box's report. Needs `Authorization: Bearer <DEVBOX_TOKEN>`;
  refused with 503 while the server has no token.
- `GET /healthz`: `{"status":"ok","version":"<commit>"}`.

`tailscale serve` sets `Tailscale-User-Name` for requests from people, which the home page shows.
It is for display only, not for authorization.

## Configuration

Server:

- `PORT`: listen port, default `8080`.
- `DEVBOX_TOKEN`: the token dev boxes report with.
- `SOURCE_COMMIT`: the deployed commit (Coolify sets it); else Go's VCS stamp, else `dev`.

Dev box (in `~/.config/intelligence/devbox.env`):

- `DEVBOX_SERVER`: the server's base URL.
- `DEVBOX_TOKEN`: the server's dev box token.
- `DEVBOX_NAME`: optional; default the lowercased hostname.
- `DEVBOX_INTERVAL`: optional; default `15s`.
- `DEVBOX_PASEO`: optional paseo executable; default `paseo` on PATH.

## Deploy

- Server: Coolify builds the `Dockerfile` and redeploys on every push to `main`. The app has no
  public domain: its port is bound to the host's loopback, and `tailscale serve` publishes it over
  HTTPS to the tailnet only. Host details are kept outside this repo.
- Dev box: from a checkout of `main` on the box, run `deploy/install-devbox` (the first run needs
  `DEVBOX_SERVER` and `DEVBOX_TOKEN` in the environment). Run it again to update the client; it does
  not update itself. Logs: `journalctl --user -u intelligence-devbox`.
