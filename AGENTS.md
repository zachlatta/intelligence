# Agent instructions

Read `README.md` first.

## Code

- Go, test-first: write the failing test, watch it fail, then make the change.
- `make test` (gofmt, `go vet ./...`, `go test -race ./...` against a throwaway Postgres) passes
  before every commit.
- Keep `main` deployable: every push to `main` deploys to production.
- Prefer the standard library. Add a dependency only when it clearly earns its place.
- Do all work in the repo, never in `/tmp`. Transient scratch goes in `.factory-scratch/` (git-ignored).

## API first

Build the app API-first, as it is built test-first:

- **Reads are SQL.** Anything a page or a client needs to know is readable with read-only SQL
  (`/api/sql`). A new thing to read is a table, view or column, not a new read endpoint.
- **Writes are API calls.** Every change goes through a JSON route under `/api/`, written and tested
  before any page uses it. The web interface makes its changes by calling those routes from the
  browser, and has no write path of its own (no form posts to page routes).

## The read-only SQL contract

All app state is readable with a read-only SQL query (`/api/sql`). So:

- State lives in Postgres, in tables or views in the `public` schema, created by a migration in
  `internal/db/migrations/`. Never keep state only in memory, in files or in another schema.
- `db.CheckContract` checks it: every relation in the database is in `public` and readable by the
  read-only role, and that role cannot write. It runs in the tests, in the image build (a failure
  fails the build, so nothing deploys) and when the server starts (a failure stops the server).
- A change that weakens the check or the read-only role is a blocking finding.

## Repo hygiene

The repository is public. Never commit secrets, hostnames, tailnet names, IP addresses or
home-directory paths; `internal/hygiene` and the CI gitleaks job check for them.
Configuration comes from environment variables set in Coolify.

Never commit agent output here: reports, audits, metrics, data exports or `work/` folders. Shared
skills that say to keep outputs in `work/YYYY-MM-DD-.../` mean a private repository; in this one,
keep them in `.factory-scratch/` and deliver them privately. `internal/hygiene` fails when Git tracks
`work/`, `scratch/`, `.factory-scratch/`, `.factory-evidence/` or a data file (CSV, spreadsheet,
Parquet and similar), or when a commit not yet on `origin/main` adds one, even if a later commit
deletes it: rewrite that commit before pushing.
