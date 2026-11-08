# Intelligence

A web app, inspired by agent-factory. For now it serves a home page and `/healthz`.

## Develop

```sh
make test   # gofmt, go vet, go test -race: must pass before every commit
make run    # http://localhost:8080
```

## Routes

- `GET /`: home page. Greets the tailnet user from the `Tailscale-User-Name` header that
  `tailscale serve` sets. The header is for display only, not for authorization.
- `GET /healthz`: `{"status":"ok","version":"<commit>"}`.

## Configuration

- `PORT`: listen port, default `8080`.
- `SOURCE_COMMIT`: the deployed commit (Coolify sets it); else Go's VCS stamp, else `dev`.

## Deploy

Coolify builds the `Dockerfile` and redeploys on every push to `main`. The app has no public
domain: its port is bound to the host's loopback, and `tailscale serve` publishes it over
HTTPS to the tailnet only. Host details are kept outside this repo.
