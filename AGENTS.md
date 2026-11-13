# Agent instructions

Read `README.md` first.

## Code

- Go, test-first: write the failing test, watch it fail, then make the change.
- `make test` (gofmt, `go vet ./...`, `go test -race ./...` against a throwaway Postgres) passes
  before every commit.
- Builders and reviewers run heavy suites through the host-wide lease: use `make test`, including
  with `TEST_DATABASE_URL` set. For a custom full suite, use `scripts/with-test-capacity go test
  -race -timeout=30m ./...`. The whole web package is also heavy: run it through that wrapper.
  Narrow package tests with `-run` can run directly while iterating. Do not bypass the lease with
  an unwrapped full `go test -race ./...`.
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
- Name a new migration `YYYYMMDDTHHMM_lower_snake_description.sql` with the current UTC time
  (`date -u +%Y%m%dT%H%M`), never a four-digit number: `0001` to `0049` are legacy and closed. Never
  rename a migration that has landed on `main`; its name is its record in `schema_migrations`.
  When merging `main`, keep both sides' migrations as they are. README's State section has the rules.
- `db.CheckContract` checks it: every relation in the database is in `public` and readable by the
  read-only role, and that role cannot write. It runs in the tests, in the image build (a failure
  fails the build, so nothing deploys) and when the server starts (a failure stops the server).
- A change that weakens the check or the read-only role is a blocking finding.

## What's New

Every change that ships a feature people can see in Intelligence adds an entry to
`internal/whatsnew/entries.json` in the same change (newest first): a new page, tab, button,
workflow ability, integration or visible behaviour. Fixes, refactors and internal changes do not.
Give it a short plain summary of what the feature is and where to find it, and a demo in
`internal/whatsnew/media/`: every entry has one (the tests fail without it). Make it a GIF of the
feature in use, or a screenshot of where it shows if nothing moves, captured from a local instance
with throwaway data, under 1.5 MiB. Never change a released entry's `id`. README's What's New
section has the format.

## Repo hygiene

The repository is public. Never commit secrets, hostnames, tailnet names, IP addresses or
home-directory paths; `internal/hygiene` and the CI gitleaks job check for them.
Configuration comes from environment variables set in Coolify.

Never commit agent output here: reports, audits, metrics, data exports or `work/` folders. Shared
skills that say to keep outputs in `work/YYYY-MM-DD-.../` mean a private repository; in this one,
keep them in `.factory-scratch/` and deliver them privately. `internal/hygiene` fails when Git tracks
`work/`, `scratch/`, `.factory-scratch/`, `.factory-evidence/` or a data file (CSV, spreadsheet,
Parquet and similar), or when a commit not yet on `origin/main` (a merge included) adds one, even if a later commit
deletes it: rewrite that commit before pushing.
