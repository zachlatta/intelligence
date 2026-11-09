# Proposal: suggest a frontier model from usage

**Add an advisory model suggestion to the existing workflow step output. Use the existing subscription meters, prefer capacity that will expire unused, and record the orchestrator's final choice.** Keep fixed rules and a human-approved frontier quality floor.

## 1. Build on the current app

The goal is to spread suitable work across subscriptions without exhausting one plan. Reuse these parts of Intelligence:

- `internal/db/migrations/0015_workflows.sql` defines `workflows`, `workflow_versions`, `workflow_runs`, and `workflow_run_events`. A run pins its definition version. Its `step` is a zero-based index; each definition step has an `id` and `kind`. Runs already keep `host`, `orchestrator_id`, and `status`.
- `internal/workflow/definition.go` defines `Step`, `StepMessage`, and `OrchestratorPrompt`. `Project` and `Orchestrator` already carry `Provider`, `Model`, and `Thinking`. Add an optional model policy to `Step` for its workers. Do not change the finder or orchestrator's explicit model choice.
- `internal/workflow/engine.go:Engine.move` records movement events and prints the newly entered step for next, back, and skip. Append a suggestion to that step output; ask only records a question and tells the orchestrator to wait. `internal/workflow/cli.go:CLI` already prints the server's returned message through `intelligence workflow next`.
- `internal/db/migrations/0006_usage.sql` supplies `usage_accounts`, `usage_readings`, `usage_active`, and `usage_sources`. Use `usage_current.remaining_pct` and its existing 15-minute `stale` flag. Use `usage_pace.pace` to identify capacity likely to remain unused at reset. `usage_cycles` and `usage_totals` provide historical context.
- `internal/db/migrations/0014_provider_catalog.sql` exposes `provider_models` and `provider_modes`. Select by the run host and exact `provider_models.provider`, `id`, and supported `thinking`. A catalog default is not proof that a model meets the frontier quality floor.

The app already polls every host every five minutes. `internal/usage/usage.go:Reader.Read` calls `cswap list --json`, the Codex subscription meter, `muse usage --json`, and `agy -p /usage --output-format json`. Muse usage is parsed by `parseMuse`; quota-exhausted errors are a fallback signal. Do not add a second collector or quota history system.

**Plans can end.** Store an explicit eligibility end time per quota pool. If a plan ends on a known date, exclude its routes at expiry even if the catalog still lists the models. Confirm the exact cutoff before enabling default acceptance; use the start of that date conservatively if only a date is known. A fresh meter is not evidence of renewal.

## 2. Remaining signal gaps

The main gaps are model-to-quota routing and durable per-worker usage:

1. Map each host/provider/model to the real usage login and all applicable quota windows. Use `usage_active` to check which login the host currently uses. Direct Claude and Claude served through Antigravity are different quota routes. Antigravity's Gemini and third-party pools must remain separate. Do not switch shared credentials to satisfy a suggestion.
2. Record which worker actually ran for each run, step, and retry. Existing movement events do not give a complete worker model history. Preserve the actual model and effort before the live `agents` row or transcript is removed. `internal/paseo/client.go:Agent.ModelName` and `Agent.Thinking` already resolve effective runtime settings.
3. Add narrow per-agent token decoding after verifying Paseo `LastUsage` semantics. The current `Agent` decoder has no such field. Store input, output, cache read/write counts, observation time, and completeness. Keep missing counts null. Deduplicate cumulative snapshots by session/counter epoch; deduplicate per-turn counts by turn ID. Tokens explain effort, but do not reliably convert to subscription percentages.

Also record sanitized rate-limit events, retry times, outcomes, and step effort requirements. Reuse `usage_sources` for meter failures. Missing or stale readings never mean unused capacity. Keep credentials and raw credential-bearing error payloads out of SQL and logs.

## 3. Small data model extension

Add migrations under `internal/db/migrations/`. Reuse the existing workflow and usage tables. The following is a concrete column sketch, not executable SQL.

| Change | Proposed fields |
| --- | --- |
| Add `Step.ModelPolicy` in `definition.go` | Optional allowed provider/model IDs, effort, quality/capability requirements, preferred checker diversity, reserve percentage, advisory/enabled mode; saved in `workflow_versions.definition` |
| New `model_quota_routes` | ID, `account_id`, host, provider, model ID, thinking, model family, usage provider/login, applicable window IDs, enabled, `eligible_until`; unique route identity |
| New `workflow_model_suggestions` | ID, `account_id`, `run_id` referencing `workflow_runs(id)`, step index and definition step ID, visit number, timestamp, policy version, candidates/exclusions/scores/input snapshot jsonb, suggested route, reason |
| Extend `workflow_run_events` | Add nullable `data` jsonb for typed event details; kinds `model_choice`, `model_override`, `worker_started`, `worker_finished`, `model_rate_limited`; existing run/step/time fields supply ordering |
| New `workflow_agent_usage` | ID, `account_id`, run ID, step, visit, worker/host reference, session/turn key, counter epoch, observation time, nullable nonnegative token counts, completeness; unique source observation key |

A choice event records suggestion ID, chosen route, requested and actual model/effort, worker ID, actor, and override reason. Worker start/end events preserve history without a duplicate step execution system. A rate-limit event records quota route, sanitized code, and retry time. Revisited steps get distinct visits. Index suggestions and events by run/step/time, and routes by account/host/model. Keep historical records independent of replaceable live agent rows.

Every new table has `account_id bigint NOT NULL REFERENCES accounts`, row-level security enabled, and `own_rows FOR SELECT USING (account_id = current_account_id())`, matching the workflow tables. Validate that referenced runs and routes have the same account on every write; add composite account-aware keys where needed. Existing usage and host tables retain their current ownership policies. Do not weaken them.

Keep all state in `public`, readable through `/api/sql`. New views must use `security_invoker = true`. Keep `db.CheckContract` and the read-only role unchanged. Writes go through tested JSON API routes under `/api/`; the UI uses those routes. Do not add read endpoints for state already available through SQL.

## 4. Suggestion rule and worked example

1. Resolve the step's optional policy against `provider_models` on the run host. Honor an explicit valid model requirement. Otherwise retain only approved frontier models with the required effort, tools, and context. A sign-off that only asks Zach needs no worker suggestion.
2. Exclude expired routes, unavailable providers, exhausted windows, and active rate-limit cooldowns. For quota ranking, require every mapped window to have a non-null `usage_current.remaining_pct` and `stale = false`. Use only the existing 15-minute freshness rule. If routing or readings are incomplete, explain the gap and show a low-confidence policy default rather than assume full capacity.
3. Let H be the minimum remaining percentage across applicable windows, minus the configured reserve. Exclude a route if H is not positive. Let P be the minimum applicable `usage_pace.pace`, clamped to 0..100; use zero when pace is unavailable. Let R be its share of the last 20 recorded worker starts within seven days. Let B be 1 if a checker shares the builder's model family, else 0.
4. Rank by `H + 0.2*P - 15*R - 10*B`. Prefer headroom, then spending before reset, then diversity. Break ties by oldest last use and stable route ID. These weights are initial policy, not measured optimums. Require a fresh checker agent even when only one model family is available.
5. Persist candidate inputs, exclusions, score terms, and reason. If a launch hits a rate limit, record the event and retry time, then suggest the next eligible option. Without a retry time, use a five-minute initial cooldown and recheck. Never silently enable paid overage or restart completed work.

Example: a checker follows a Claude builder. All candidates meet the step requirements. H includes a common reserve. Values are illustrative, not live balances.

| Option | H | P | R | B | Score |
| --- | ---: | ---: | ---: | ---: | ---: |
| Direct Claude | 12 | 0 | 0.50 | 1 | -5.5 |
| Codex | 65 | 20 | 0.30 | 0 | 64.5 |
| Gemini via Antigravity | 40 | 30 | 0.20 | 0 | 43.0 |

Suggest Codex: 65 points of limiting-window headroom after reserve, positive pace, and a different model family. If Codex is rate-limited before launch, suggest Gemini while its plan remains eligible. Antigravity-hosted Claude does not count as a different family from a Claude builder.

## 5. Workflow behavior

Append a short block to the next step message in `Engine.move`: “Suggest Codex, medium effort. Limiting-window headroom after reserve: 65%. Capacity is being used slower than the window. Checker differs from Claude builder.” Include suggestion ID, exact catalog model ID, reading age, and confidence. Use the same helper for the first step in `OrchestratorPrompt`, backward moves, and resumed steps.

`next` already completes the current step; it is not a read-only suggestion command. Preserve that meaning. Generating the appended suggestion must not launch a worker. The orchestrator still launches workers through Paseo. Add an API-first choice/report action, with CLI support, to record acceptance or an override and the resulting worker ID. Recheck expiry, routing, and meter freshness before launch. Record runtime settings after launch and outcomes before archive. Overrides change preference, but cannot make an expired or exhausted route eligible.

Several runs can choose the same capacity between polls. In advisory rollout, show this limit and count recent worker starts in the reason. Before default acceptance, add short-lived, account-scoped route allocation leases in Postgres. Serialize accepted starts per shared quota pool, reserve a conservative fixed allowance, and release or expire leases on finish, failure, or crash. Reconcile leases after restart. Do not infer a precise token-to-quota conversion.

## 6. Tests and rollout

For each code change, write the failing Go test, observe failure, then implement. Use a fake clock to test the existing 15-minute freshness boundary, reset-to-null behavior, unknown routing, shared pools, plan expiry, effort eligibility, pace, diversity, ties, and rate-limit fallback. Test LastUsage counter deduplication and reset handling with fake Paseo data.

Add database and CLI tests for run/step/visit linkage, first-step and `Engine.move` output, override persistence, archive survival, and cross-account isolation. Verify every new relation through `db.CheckContract`, verify account A cannot read account B, and verify SQL writes fail. Test allocation races and lease recovery before default acceptance. Run `make test` before every commit.

1. Validate existing readings, quota mappings, catalog IDs, and expiry dates. Begin advisory suggestions on one workflow without changing explicit model defaults.
2. Record choices and outcomes. Compare quota failures, elapsed time, completion rates, and checker findings by step kind. Do not claim quality differences from small samples.
3. After policy approval and lease tests, allow default acceptance. Keep overrides and a persisted switch back to advisory mode. Do not delay expiry handling for a seven-day observation period.

## 7. Open questions

Should a checker require a different model family, or only prefer one? What reserve should remain for interactive work? I recommend advisory rollout with a reserve and automatic exclusion at expiry.

Resolve these policy choices before default acceptance.
