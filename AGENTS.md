# Agent instructions

Read `README.md` first.

## Code

- Go, test-first: write the failing test, watch it fail, then make the change.
- `make test` (gofmt, `go vet ./...`, `go test -race ./...` against a throwaway Postgres) passes
  before every commit.
- Keep `main` deployable: every push to `main` deploys to production.
- Prefer the standard library. Add a dependency only when it clearly earns its place.
- Do all work in the repo, never in `/tmp`. Transient scratch goes in `.factory-scratch/` (git-ignored).

## The read-only SQL contract

All app state is readable with a read-only SQL query (`/api/sql`). So:

- State lives in Postgres, in tables or views in the `public` schema, created by a migration in
  `internal/db/migrations/`. Never keep state only in memory, in files or in another schema.
- `db.CheckContract` checks it: every relation in the database is in `public` and readable by the
  read-only role, and that role cannot write. It runs in the tests, in the image build (a failure
  fails the build, so nothing deploys) and when the server starts (a failure stops the server).
- A change that weakens the check or the read-only role is a blocking finding.

## Repo hygiene

Never commit secrets, hostnames, tailnet names, IP addresses or home-directory paths.
Configuration comes from environment variables set in Coolify.
