# Agent instructions

Read `README.md` first.

## Code

- Go, test-first: write the failing test, watch it fail, then make the change.
- `make test` (gofmt, `go vet ./...`, `go test -race ./...`) passes before every commit.
- Keep `main` deployable: every push to `main` deploys to production.
- Prefer the standard library. Add a dependency only when it clearly earns its place.
- Do all work in the repo, never in `/tmp`. Transient scratch goes in `.factory-scratch/` (git-ignored).

## Repo hygiene

Never commit secrets, hostnames, tailnet names, IP addresses or home-directory paths.
Configuration comes from environment variables set in Coolify.
