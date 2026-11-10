# Frontier model selection in workflows: audit and proposal

Investigation of 2026-11-10 (workflow run `b8f0791f`). The [reproduction notes](https://files.zachlatta.com/c7b545ad-284d-4ea4-bc4c-7457bbed7cbc/work-README.md) describe the scripts and counts. Scripts live in the private desk work item `work/2026-11-10-frontier-model-selection/`.

## Summary

- **Selection is not random. It is an LLM's reading of static prose, and it splits about
  50/50 between Claude and Codex.** No code and no prompt reads a usage meter. Of the 311
  workflow sub-agents (2026-11-09 to 2026-11-10), 163 (52%) ran on Claude Opus 5.5, 144 (46%) on
  GPT-6.1 Sol, 4 (1%) on Antigravity and 0 on Muse.
- **The five-model list in the Factory Task's first step has almost no effect.** The
  orchestrator's standing instructions say "spread the work across the plans:
  claude/claude-opus-5-5 and codex". Orchestrators follow that line for most launches. Of the
  4 Antigravity launches, 3 came from tasks that named Gemini or Antigravity. The fourth was this
  investigation's builder: the orchestrator chose it from the five-model list after it looked at
  plan usage. That is the only sampled case of the list working as intended. No orchestrator
  chose Muse.
- **The orchestrator is already fixed.** All 102 orchestrators and all 54 project finders ran on
  `claude/claude-opus-5-5` (84 at medium thinking, 18 at high).
- **Remaining capacity is uneven now.** At 2026-11-10 19:07 UTC:
  - Claude login 2 (Ultra) had used 0% of a week that was 47% over.
  - Codex had 59% of its week left and 12 hours until the reset. Most of it will be left unused
    unless spending rises sharply (a conditional forecast).
  - The Antigravity Gemini pool had 93% left, and the Google plan ends 2026-11-13 or 2026-11-14.
  - Muse's meter returned no data (`server_sent_no_usage`), so its use is unknown, not zero. No
    workflow agent ran on Muse. The plan ends 2026-11-13.
- **Model choice cannot fix the biggest gap.** The two Claude logins are one Paseo provider.
  cswap moves every agent on the host to the other login only when the active one reaches 90%.
  So, in a slow week, login 2 is not used at all.
- **Recommendation:** keep the orchestrator pinned in the workflow definition. Compute a
  per-role suggestion for sub-agents from a SQL view over the meters that Intelligence already
  collects. The view subtracts the expected spend of recent suggestions, so a burst of launches
  spreads across plans. Put the suggestion, as an exact `paseo run` line, into each step message.
  If no plan has established capacity, say "wait" instead of guessing. Record which model each
  worker actually ran on with Paseo labels. Section 5 gives the flow and the rollout.

## 1. How a model is chosen today

### 1.1 Every place that picks or suggests a model

| Where | What it says | Effect |
| --- | --- | --- |
| [`definition.go:254`](../../internal/workflow/definition.go#L254) `Starting.Orchestrator` | `claude` / `claude-opus-5-5` / `high` | The orchestrator. The server starts it with exactly these settings ([`engine.go:429-432`](../../internal/workflow/engine.go#L429-L432)). Saved definitions override the default: 84 of the 102 orchestrators ran at `medium`. |
| [`definition.go:252`](../../internal/workflow/definition.go#L252) `Starting.Project` | `claude-opus-5-5`, `medium` | The project finder. All 54 finders ran on it. |
| [`definition.go:242`](../../internal/workflow/definition.go#L242) `OrchestratorInstructions` | Example: `paseo run ... --provider claude/claude-opus-5-5 ...` | This is the only full command in the prompt, so it anchors the choice. The saved Factory Task copy has no `--mode` in the example. |
| [`definition.go:246`](../../internal/workflow/definition.go#L246) | "Use frontier models, and spread the work across the plans: claude/claude-opus-5-5 and codex." | The rule that orchestrators actually follow. It names two plans and no thinking level. |
| [`definition.go:217-220`](../../internal/workflow/definition.go#L217-L220) workflow contract | "Follow this workflow's model choices rather than a shared skill's default." | This puts the workflow text above start-here. If a workflow names no model, the skill default applies. |
| [`definition.go:186-194`](../../internal/workflow/definition.go#L186-L194) `subAgentModes` | Gives the unattended `--mode` for each provider ([`modes.go:4-9`](../../internal/paseo/modes.go#L4-L9)) | Gives modes only. It says nothing about models or thinking. |
| Saved "Factory Task", step 1 (in `workflow_versions.definition`) | "Pick a frontier model out of the following to intelligently ensure we are using all of our subscriptions: Claude Opus 5.5 (Medium), Gemini Argon 4 (Medium), Gemini Opus 5.5 (Medium), GPT 6.1 Sol (High), Muse Wonder (High)" | Applies to step 1 only. It uses display names, not Paseo ids. One earlier version (1 run) said "GPT 6.1 Sol (Medium)". |
| Saved "Zach Email Task" / "Email Task" (44 runs) | Names no model | In 43 of the 44 runs, the orchestrator did the work itself on Opus. One run (`f50f17c8`) started 3 Opus workers detached (`env -u PASEO_AGENT_ID`, labels `run=f50f17c8`, `role=builder`), so they have no Paseo parent. |
| Saved staff-email workflow (4 runs) | The `Starting` instructions unchanged | The same two-plan line. |
| [`spawn.go:21-23`](../../internal/web/spawn.go#L21-L23), [`README.md:633`](../../README.md#L633), [`README.md:650`](../../README.md#L650) | Spawn and schedule forms start at Opus 5.5, medium, `bypassPermissions` | The schedules: all 1,050 schedule runs used Opus. |
| `start-here` skill, lines 34-36 | "Every agent you launch (Paseo run, schedule, subagent) uses Opus 5.5 (`claude/claude-opus-5-5`) unless Zach names another model" | This is the default for every agent outside a workflow that names models. |
| `paseo-cli` skill, "Default model" | Default `claude/claude-opus-5-5 --thinking medium`. "Honor Zach's or an assigned workflow's model and thinking choices, including spreading across plans." | Agrees with the contract. |
| Retired agent-factory board, `factory/agents.go:14-15`, `:35-48`, `:126-250` (agent-factory repo) | Reviewers are fixed on Opus. The builder is picked in code: available frontier models, ranked by weekly pace with an 0.8 weight on Claude and Codex. | This was the only usage-aware picker. It is retired, and Intelligence did not inherit it. |
| Intelligence plan usage: `usage_current`, `usage_pace`, `usage_cycles`, `usage_totals` ([`0006_usage.sql:63-105`](../../internal/db/migrations/0006_usage.sql#L63-L105)) | Every login's windows, pace and weekly peaks, read every 5 minutes | Nothing in the workflow path reads them. |
| `provider_models` ([`0014_provider_catalog.sql:7-20`](../../internal/db/migrations/0014_provider_catalog.sql#L7-L20)) | Each host's models and their thinking levels | Nothing in the workflow path reads it. |

### 1.2 Contradictions between these sources

1. **Two plans or five?** The standing instructions name Claude and Codex. Step 1 of the Factory
   Task names five options on four plans. Orchestrators resolve this in favor of the standing
   instructions. Their own words: "The workflow mentions spreading work across Claude Opus and
   Codex, so I need to pick a builder."
2. **Skill or workflow?** start-here says that every agent uses Opus. The contract says to
   follow the workflow. Orchestrators notice this ("Start-here says every agent defaults to Opus
   5.5 ... but the workflow card lists specific models"), and the contract wins. In the email
   workflows, the workflow names no model, so the skill wins.
3. **Display names are not Paseo ids.** "Gemini Opus 5.5 (Medium)" is Claude Opus served by
   Antigravity (`antigravity/claude-opus-5-5-medium`). It is the same model family as Claude
   direct, with a different and much smaller quota pool: about $25 of Opus a week (the
   `ai-subscription-limits` skill). "Muse Wonder (High)" maps to a real setting: Paseo lists
   `minimal` to `max` thinking for both Wonder ids (`muse-wonder-full-1.0`, `muse-wonder-1.0`),
   with `max` as the default. So a launch without `--thinking high` runs at `max`, not at the
   level the list asks for. The two Wonder ids are themselves ambiguous. "Gemini Argon 4 (Medium)"
   differs from the board's old default (`gemini-4-argon-high`).
4. **Thinking levels are lost.** The list asks for Sol at High and Opus at Medium. In practice:
   - 128 Opus sub-agents started with no `--thinking` and ran at the provider default.
   - 40 Codex sub-agents started without `--thinking` (most as bare `--provider codex`), and they
     ran at `low`.
5. **Orchestrator thinking.** `Starting` uses `high`, but the saved Factory Task uses `medium`.
6. **Side finding:** `{{input}}` is not filled in orchestrator instructions. `Vars.fill` runs
   only on step text ([`definition.go:156-158`](../../internal/workflow/definition.go#L156-L158),
   [`:201`](../../internal/workflow/definition.go#L201)). The email workflows put `{{input}}` in
   their orchestrator instructions, so the orchestrator receives the literal text `{{input}}`.
   The input still arrives under "What started it".

## 2. Sampling

Sources:

- The Paseo agent store on the workflow host: 6,186 agent records, archived ones included,
  created 2026-10-24 to 2026-11-10.
- 102 orchestrator transcripts with 322 `paseo run` launch commands. Duplicate log entries are
  removed. 11 further matches had no `--provider`: they were text that mentions `paseo run` (3
  `grep` commands in one run's step 1, and 8 prompts written for sub-agents), not launches.
- PDW `marts_ai_conversations.sessions`, used as a cross-check.
- The live meters.

The sample is frozen at 2026-11-10 19:05 UTC (`CUTOFF` in `analyze.py`). Runs that started
later do not change these counts. A workflow sub-agent is an agent below an orchestrator in
Paseo's parent chain (308), or an agent whose `intelligence.run` or `run` label names a run (3
more, all detached workers of run `f50f17c8`): 311 in total. Workers with neither a parent nor a
label cannot be found, so 311 is a lower bound.

Intelligence's own `/api/sql` was not reachable (section 6). Full tables:
[`sampling.md`](https://files.zachlatta.com/c7b545ad-284d-4ea4-bc4c-7457bbed7cbc/sampling.md).

### 2.1 By role

| Role | Agents | Claude Opus 5.5 | GPT-6.1 Sol | Antigravity | Muse |
| --- | ---: | ---: | ---: | ---: | ---: |
| Workflow orchestrator | 102 | 102 (100%) | 0 | 0 | 0 |
| Workflow project finder | 54 | 54 (100%) | 0 | 0 | 0 |
| Workflow sub-agents, all | 311 | 163 (52%) | 144 (46%) | 4 (1%) | 0 |
| - builders (role label, else title) | 65 | 29 | 32 | 4 | 0 |
| - checkers (by title) | 193 | 96 | 97 | 0 | 0 |
| - land / rebase | 42 | 32 | 10 | 0 | 0 |
| Factory board builders (per-card picker, retired) | 1,598 | 807 (51%) | 597 (37%) | 178 (11%) | 14 (1%) |
| Factory board reviewers and scouts | 2,410 | 2,410 (100%) | 0 | 0 | 0 |
| Paseo schedule runs | 1,050 | 1,050 (100%) | 0 | 0 | 0 |
| All agents | 6,186 | 5,188 (84%) | 778 (13%) | 189 (3%) | 17 (0.3%) |

In the Factory Task's step 1, where the five-model list applies, orchestrators wrote 64 launch
commands: 23 Claude, 37 Codex, 4 Antigravity and 0 Muse. 3 of the 4 Antigravity launches came
from tasks that asked for Gemini or Antigravity by name: 2 Argon builders for a video montage,
and 1 Antigravity Opus builder for a plan-subsidy analysis. The fourth (run `b8f0791f`) is this
investigation: the request names neither. The orchestrator chose Antigravity Opus from the
step's list after it saw plan usage. So the list can work, but in the sample it changed the
choice once. The standing two-plan sentence and the task text decided the rest. (The first version of this report counted
67. The 3 extra commands were `grep` searches for the text "paseo run", not launches.)

The code-based board picker spread work more widely (11% Gemini Argon) than the LLM
orchestrators (1%).

### 2.2 Over time

The share of new agents that did not run on Claude rose from 3-7% (2026-10-25 to 10-28) to a
median of 20% on 10-29 to 11-10 (range 4-34%; most days 14-34%, with 16% on 11-04 and 4% on
11-05). Codex grew most. Antigravity peaked at 55 agents on 2026-11-06 under
the board picker, then fell to 1-3 a day after workflows took over on 2026-11-09.

### 2.3 Checker diversity

There were 47 runs with both a builder and a checker. In 9 of them, every checker used a
different model family from every builder. In 36, families overlapped. In 2, every checker
family was also a builder family. Orchestrators often reason "fresh checker on the other plan".
No rule requires this. Every checker ran on Claude or OpenAI. The only Gemini-family workers
were 2 Antigravity Argon builders. The other 2 Antigravity builders ran Claude Opus, which is
the Anthropic family.

### 2.4 Plan meters: live snapshot (2026-11-10 19:07 UTC)

| Plan | Used | Window resets (UTC) | Window elapsed | Plan ends | Remaining capacity and forecast |
| --- | ---: | --- | ---: | --- | --- |
| Claude login 1 (Ultra) | 59% (7d) | 11-13 11:00 | 62% | renews | 41% left. On pace (cswap: `willLastToReset`). |
| Claude login 2 (Ultra) | **0%** (7d) | 11-14 12:00 | 47% | renews | 100% left. Idle: cswap switches only at 90% on login 1. |
| ChatGPT Pro (Codex) | 41% (7d) | 11-11 06:48 | 93% | renews | **59% left** with 12 h to go. If spending continues at the week's rate (about 6% a day), about 56% will be unused at the reset. |
| Antigravity, Gemini pool | 7% (7d), 0% (5h) | 11-14 00:00 | about 54% | 11-13 or 11-14 | **93% left.** It can be used only until the plan ends. At this week's rate, nearly all of it will be unused. |
| Antigravity, Claude/GPT pool | 61% (7d), 18% (5h) | 11-16 02:34 | about 25% | 11-13 or 11-14 | 39% left. Small pool. This investigation's builder ran on it. |
| Muse Code Power | unknown: the meter returned `server_sent_no_usage` | unknown | unknown | 11-13 | Unknown. The CLI says this can happen before the first use in a window. No workflow agent ran on Muse. |

"Unused" in this table is a conditional forecast: it assumes spending continues at the recent
rate. The quota is still available until the reset or the plan end.

Plan end dates come from the `ai-subscription-limits` skill (Google: the cancel notice said
Nov 13, a later reminder said Nov 14). Verify them before you rely on them.

**Quota incident after the sample cutoff (2026-11-10).** At about 19:55 UTC, this
investigation's Antigravity Claude Opus builder stopped with "Individual quota reached" and
a reset about 126 hours away (reported by the orchestrator at handoff). A read-only
`agy -p /usage --output-format json` check at about 20:00 UTC confirmed **0% remaining** in
`3p-weekly`, resetting 2026-11-16 02:34:03 UTC; `3p-5h` was disabled because the weekly pool
was exhausted. Gemini still had 92.55% remaining for the week (displayed as 93%) and 100%
for five hours. The command used zero model turns. The mostly unused Gemini meter therefore
did not imply capacity for Opus: the meter exposes separate pools, and the Claude/GPT pool
is much smaller (see 1.2). The 19:07 snapshot already showed that pool 61% used. One long
worker can exhaust the rest between readings. This is exactly why the orchestrator needs
the proposed mid-run `suggest --exclude agy-opus --why "Individual quota reached"` fallback
(5.4-5.5), rather than retrying the same route. This incident does not change the frozen
19:05 agent sample or the worked example based on the 19:07 meters.

### 2.5 Plan meters: history

- **Claude** ([`claude_weeks.md`](https://files.zachlatta.com/c7b545ad-284d-4ea4-bc4c-7457bbed7cbc/claude_weeks.md)). cswap
  switched logins 4 times from 10-28 to 11-07. Each switch came when the active login reached
  90%. In the weeks to 10-31 and 11-07, both logins reached 90-94%. This week, login 1 is at 59%
  and login 2 is at 0%. Demand fell: 853 new agents on 11-06, and 117-398 a day after the board
  was retired.
- **Codex** ([`codex_weeks.md`](https://files.zachlatta.com/c7b545ad-284d-4ea4-bc4c-7457bbed7cbc/codex_weeks.md)). In the
  completed week to 11-04, the highest reading seen was 68%, so about 32% of that week was not
  used. In the current week to 11-11, 41% is used and 59% remains. These are host-local readings
  of the account-wide meter.
- **PDW cross-check** (sessions since 10-25): 5,460 Opus sessions on the workflow host, 672 Codex
  `gpt-6.1-sol`, 182 + 9 Gemini Argon, 82 Muse Spark and 18 Muse Wonder. This agrees with the
  agent store.

## 3. Audit

**Is it random, biased or deterministic?** It is biased. It is not random, and it is not
usage-aware.

- The choice is the orchestrator's judgment. Its inputs are the two-plan sentence, the Opus
  example command and a habit to "put the checker on the other plan".
- The result is close to 50/50 between Claude and Codex in every step, with Claude favored for
  landing (32 of 42).
- The list's other three options get about 1%.

**Which plans are used too little or too much for their capacity?**

- **Too little:**
  - Gemini Argon: 93% left, usable only until the plan ends.
  - Codex: 59% left with 12 hours to go, and about 32% of the last completed week unused.
  - Claude login 2 in slow weeks: 0% used this week. The skill estimates an Ultra week at about
    $2,390 of Opus at API prices.
- **Unknown:** Muse. Its meter returned no data, and no workflow agent ran on it. The factory
  board and its sub-agents ran 17 Muse agents, the last on 11-06.
- **Correctly or too much:** Claude login 1 (on pace, and it carries every orchestrator,
  schedule and interactive session) and the Antigravity Claude/GPT pool (small, 61% used).

**Failure modes found:**

1. **Lost thinking levels.** See 1.2 item 4. Bare `--provider codex` gives Sol at `low`, which is
   below the quality floor that the step names.
2. **Display names that Paseo does not know.** The orchestrator must look up ids with
   `paseo provider models`. Several orchestrators spent turns doing this, and some then
   discarded the choice.
3. **Mode mismatch.** One launch failed: `Invalid mode 'bypassPermissions' for provider 'codex'`.
   Four Claude launches had no `--mode`. These were rare, because `subAgentModes` adds modes to
   the prompt.
4. **Provider stalls and mid-run quota exhaustion.** Before the sample cutoff, the quota-error
   matches (`RESOURCE_EXHAUSTED`, 429) were all quoted skill text. At about 19:55 UTC this
   investigation's Antigravity Opus builder exhausted its weekly pool; the 20:00 meter check
   confirmed it while Gemini remained mostly unused (2.4). A launch-time check alone is not
   enough: a running worker can exhaust a small pool and needs a different eligible route.
   Muse's CLI usage endpoint returned 401 on 11-07 (skill), so its meter may be unreliable.
5. **Herding.** Runs start in bursts, and the meters refresh every 5 minutes. A picker that only
   reads meters sends a burst to the same plan. Section 5.3 adds a penalty for this.
6. **No record of role.** The Paseo `agents` table has `parent_id`, `model` and `thinking`, but
   no labels ([`0003_workspaces.sql:39-55`](../../internal/db/migrations/0003_workspaces.sql#L39-L55)).
   The host already decodes `Labels` ([`client.go:86`](../../internal/paseo/client.go#L86)) and
   drops them. Builder versus checker is known only from free-text titles. The parent chain
   is not reliable either: run `f50f17c8` started 3 workers with `env -u PASEO_AGENT_ID`, so
   Paseo recorded no parent, and only their own `run`/`role` labels tie them to the run. The
   first version of this sample missed them for that reason. Storing labels in SQL
   (`0029_agent_labels.sql`, 5.2) fixes both problems.

## 4. Options

| Option | How it works | Good | Bad |
| --- | --- | --- | --- |
| A. Static list in prompts (today) | Prose in instructions and steps | No code | Ignored, contradictory and goes stale. No feedback. Thinking levels get lost. |
| B. Weighted rotation | Round-robin over routes with fixed weights | Easy, deterministic | Ignores real use (interactive, schedules), resets and plan ends. Weights go stale every week. |
| C. Usage-aware picker in Go | Engine code reads meters and ranks | Uses live data. The board ran this for 2 weeks. | Hidden logic. Its inputs are not readable as SQL. Policy changes need a deploy. |
| D. Route table + ranking view + suggestion in step text (recommended) | `model_routes` table (policy), `model_route_status` view (end-aware score from `usage_pace` and the plan end, minus the expected spend of recent suggestions; `provider_models` for eligibility), engine appends the top choice per role to step messages, writes record each suggestion | Readable with SQL (API-first). Policy is data. Checkers are diverse by rule. Expiry is handled. Choices are auditable. | Needs two migrations, one write route and the CLI. Claude login balance still needs a cswap change. |
| E. Server launches the workers | Intelligence starts sub-agents itself | Full control | Changes the orchestrator's role. Large change. Not needed for the goal. |

## 5. Recommended flow (option D)

### 5.1 Principles

- **The orchestrator stays pinned.** Use `Definition.Orchestrator` (provider, model, thinking).
  It never enters the ranking. Recommendation: `claude/claude-opus-5-5`, thinking `high`, as in
  `Starting`. Zach picks the level.
- **Suggest sub-agent models per role:** `builder`, `checker` and `mechanical` (rebase, land).
  An explicit model in the task ("use opus 5.5 for it") always wins.
- **Reads are SQL. Writes are JSON routes.** The ranking is a view. Each suggestion is a row that
  a server write creates.
- **Unknown capacity is never treated as full capacity.**

### 5.2 Data model (migrations)

**`0029_agent_labels.sql`.** Add `agents.labels jsonb NOT NULL DEFAULT '{}'`. Store the
`Labels` that the host already decodes. `workflow_run_agents.agent` then keeps them through
archive, and `recorded_agents` exposes them. Sub-agents carry `intelligence.run`,
`intelligence.step`, `intelligence.role` and `intelligence.route`.

**`0030_model_routes.sql`.** Adds the following:

```sql
CREATE TABLE model_routes (           -- policy: the frontier routes workflows may use
    account_id         bigint NOT NULL REFERENCES accounts,
    id                 text   NOT NULL,  -- 'codex-sol-high'
    label              text   NOT NULL,  -- 'GPT-6.1 Sol (High)'
    provider           text   NOT NULL,  -- Paseo provider: codex
    model              text   NOT NULL,  -- Paseo model id: gpt-6.1-sol
    thinking           text   NOT NULL DEFAULT '',
    family             text   NOT NULL,  -- anthropic | openai | google | meta (checker diversity)
    usage_provider     text   NOT NULL,  -- usage_readings.provider this route spends
    long_window        text   NOT NULL,  -- the window that sets the score: 7d or claude-gpt/7d
    gate_windows       text[] NOT NULL DEFAULT '{}', -- short windows that only gate: {5h}
    roles              text[] NOT NULL DEFAULT '{builder,checker,mechanical}',
    capacity_weight    real   NOT NULL DEFAULT 1,   -- share workflows may spend (Claude 0.8: orchestrators, schedules, Zach)
    expected_spend_pct real   NOT NULL,             -- long-window % one worker is expected to use
    reserve_pct        real   NOT NULL DEFAULT 5,   -- never suggest below this much left
    eligible_until     timestamptz,                 -- plan end; NULL = renews
    enabled            boolean NOT NULL DEFAULT true,
    position           int    NOT NULL,             -- tie-break and editor order
    PRIMARY KEY (account_id, id)
);
CREATE TABLE workflow_model_suggestions (   -- every suggestion made, and every reported failure
    id uuid PRIMARY KEY, account_id bigint NOT NULL REFERENCES accounts,
    run_id uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
    step int NOT NULL, role text NOT NULL,
    outcome text NOT NULL,                     -- 'route' or 'wait'
    route_id text,                             -- NULL when outcome = 'wait'
    alternates text[] NOT NULL, inputs jsonb NOT NULL,       -- the status rows it ranked
    excluded jsonb NOT NULL DEFAULT '[]',                    -- routes reported failing, with why and until when
    wait_until timestamptz,                    -- outcome = 'wait': earliest time anything can change
    created_at timestamptz NOT NULL
);
-- RLS own_rows on both; views security_invoker; checked by db.CheckContract.
```

Seed rows from Zach's list, with exact ids. Every option names its thinking level, because a
launch without `--thinking` runs at the provider default (Sol: `low`, Muse Wonder: `max`).
`expected_spend_pct` starts as a rough guess. Phase 1 measures it: for each route, compare
the steps in `usage_readings` with the labelled workers started in the same hours.

| Route id | Provider and model | Thinking | Family | Long window, gates | Weight | Expected spend (initial guess) | Eligible until |
| --- | --- | --- | --- | --- | ---: | ---: | --- |
| `opus` | `claude/claude-opus-5-5` | medium | anthropic | claude `7d` | 0.8 | 0.5% | renews |
| `codex-sol-high` | `codex/gpt-6.1-sol` | high | openai | codex `7d` | 1 | 1% | renews |
| `argon` | `antigravity/gemini-4-argon-medium` | (in the model id) | google | `gemini/7d`, gate `gemini/5h` | 1 | 2% | Google plan end |
| `agy-opus` | `antigravity/claude-opus-5-5-medium` | (in the model id) | anthropic | `claude-gpt/7d`, gate `claude-gpt/5h` | 1 | 5% (small pool) | Google plan end |
| `muse-wonder` | `muse/muse-wonder-full-1.0` | high | meta | muse `7d`, gate `5h` | 1 | 3% | 2026-11-13 |

**View `model_route_status`** (`security_invoker`). It is the only read model for suggestions.
It has one row per route, host and role. Each row has the inputs below (`R`, `L`, `pace`, `H`,
`U`, `n`), `strand`, `strand_adj` and `score`, the eligibility result (5.5: `eligible`, `probe`,
`reason`) and `rank`: the row's position among its host's and role's rows, ordered by eligible
first, then `score` descending, then `position`. The engine reads it, and anyone can read it
through `/api/sql`.

### 5.3 The score: end-aware `strand` and the allocation penalty

All values are percentages of one full window. For a route's long window, at time `now`:

| Symbol | Meaning | Source |
| --- | --- | --- |
| `R` | Percent of the window left | `usage_current.remaining_pct` |
| `T` | Window length | `usage_current.window_seconds` |
| `L` | Percent of the window's time still to run: `100 × (resets_at − now) / T` | `usage_pace.window_left_pct` |
| `pace` | `R − L` | `usage_pace.pace` |
| `H` | Horizon: `resets_at` when `eligible_until IS NULL` (a renewing plan), else the earlier of the two | route and meter |
| `U` | What full-pace spending can still use before `H`: `100 × (H − now) / T` | computed |
| `n` | Suggestions for this route in the last 60 minutes, counted from `workflow_model_suggestions` | computed |
| `c` | `expected_spend_pct` | route |

Then:

```text
strand     = R − U                       (= pace when the plan does not end before the reset;
                                          = pace + 100 × (resets_at − H) / T when it does)
R_adj      = R − c × n                   (quota already promised to recent suggestions)
strand_adj = R_adj − U
score      = strand_adj × capacity_weight
```

**What `strand` means.** Suppose the route is spent from now on at "full pace", the rate that
uses 100% of a window in one window length. `strand` is the quota that would still be left at
the horizon. If `strand` is positive, even full-pace use leaves quota behind, so the route
should get more work. If it is negative, the route will run out before the horizon at full
pace. It is the same sign convention as `usage_pace`. The only change is that a plan that ends
before the reset shortens the horizon. It is a forecast that depends on future spending. It is
not quota already lost.

Short windows (`gate_windows`, such as `5h`) do not enter the score. They only gate: a route
with less than `reserve_pct` left in any gate window is not eligible until that window resets.

**Penalty and serialization.** The helper takes `pg_advisory_xact_lock(account_id)`, reads the
view and inserts its suggestion row in one transaction. So each suggestion in a burst sees the
ones before it. Each one lowers its route's `R_adj` by `c`. When a route's score drops below the
next route's, the next suggestion goes to the next route. Meters refresh only every 5 minutes,
so the penalty counts the spend that they cannot show yet. Counting it for 60 minutes is
conservative: some of that spend may already be on the meter. Phase 1 data sets the window.
Weighted random choice (probability proportional to `max(score, 0)`) also spreads bursts. But it
cannot be reproduced and tested. Use it only if the penalty proves too coarse.

**Worked example** (meters at 2026-11-10 19:07 UTC; the Google plan end is taken as
2026-11-13 00:00 UTC, the earlier of the two dates; `c` values are the initial guesses above):

| Route | R | Reset in | L | pace | H − now | U | strand | Weight | Score with n = 0 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `argon` (`gemini/7d`) | 93 | 76.9 h | 45.8 | 47.2 | 52.9 h (plan end) | 31.5 | **61.5** | 1 | 61.5 |
| `codex-sol-high` | 59 | 11.7 h | 7.0 | 52.0 | 11.7 h (reset) | 7.0 | **52.0** | 1 | 52.0 |
| `agy-opus` (`claude-gpt/7d`) | 39 | 127.5 h | 75.9 | −36.9 | 52.9 h (plan end) | 31.5 | **7.5** | 1 | 7.5 |
| `opus`, active login 1 | 41 | 63.9 h | 38.0 | 3.0 | 63.9 h (reset) | 38.0 | **3.0** | 0.8 | 2.4 |
| `muse-wonder` | unknown | unknown | | | | | | | not ranked (meter unknown) |

Check of the end-aware term for `argon`: pace 47.2 + 100 × (76.9 − 52.9) / 168 = 47.2 + 14.3 =
61.5. The plan's end makes Argon's leftover larger than its pace alone shows. For `agy-opus`,
pace is −36.9 (it would run out before its reset), but the reset comes after the plan ends, so
the quota that is left before the end still counts: 7.5.

For Claude: while cswap fails over at 90%, only the active login is reachable, so `opus` uses
login 1 (score 2.4). After the cswap balancing change (5.7), use the average over the logins.
Login 2 has R = 100, L = 52.9 and strand 47.1, so the average strand is 25.05 and the score is
20.0.

**A burst of 8 builder suggestions:**

| # | Scores before (argon / codex) | Suggested | Scores after |
| ---: | --- | --- | --- |
| 1 | 61.5 / 52.0 | argon | 59.5 / 52.0 |
| 2 | 59.5 / 52.0 | argon | 57.5 / 52.0 |
| 3 | 57.5 / 52.0 | argon | 55.5 / 52.0 |
| 4 | 55.5 / 52.0 | argon | 53.5 / 52.0 |
| 5 | 53.5 / 52.0 | argon | 51.5 / 52.0 |
| 6 | 51.5 / 52.0 | codex | 51.5 / 51.0 |
| 7 | 51.5 / 51.0 | argon | 49.5 / 51.0 |
| 8 | 49.5 / 51.0 | codex | 49.5 / 50.0 |

Without the penalty, all 8 go to Argon. With it, the burst spreads in proportion to each
route's leftover quota and expected spend per worker. Checkers then go to a different family
(5.6), so an Argon builder gets a Codex checker.

### 5.4 Where the suggestion is computed and how it reaches the orchestrator

1. `Engine.move` ([`engine.go:539`](../../internal/workflow/engine.go#L539)) and the first-step
   path in `OrchestratorPrompt` call one helper. The helper reads `model_route_status` for the run's
   host and inserts a `workflow_model_suggestions` row in the same transaction. Then it appends a
   short block to `StepMessage`. Example:

   ```text
   Models for this step (suggestion 3f2a…, meters 2 min old):
   - Builder: Gemini 4 Argon (Medium). 93% of its week is left, and the plan ends in 53 h:
     even at full pace about 61 points would be left over.
       paseo run --no-wait --workspace <id> --provider antigravity/gemini-4-argon-medium --mode full-access \
         --label intelligence.run=<run> --label intelligence.step=build --label intelligence.role=builder \
         --label intelligence.route=argon --title "..." "<task>"
   - Checker: a different family from the builder you started. Next: GPT-6.1 Sol (High), then Opus 5.5 (Medium).
   - If a launch fails or a worker reports a quota error, run
       intelligence workflow suggest --run <run> --role <role> --exclude <route> --why "<error>"
   - If the task names a model, use that model.
   ```

2. `intelligence workflow suggest` calls `POST /api/workflow-runs/{id}/suggestions` (host key,
   as for moves). This is a write: it records the exclusion and a new suggestion, and it returns
   the text. Use it for a checker round later in a long step, or after a failure. It is not a
   read endpoint: the state it returns is also in SQL.
3. Replace [`definition.go:242-246`](../../internal/workflow/definition.go#L242-L246) with: "Start
   each sub-agent with the model the step's suggestion names, unless the task names one. Pass
   `--thinking` and the labels exactly. If the suggestion says wait, wait." Remove the static
   lists from saved definitions. Then the prompt has only one source of truth.

### 5.5 Eligibility and fallbacks

A route is **eligible** for a run, step and role only when all of these are true:

1. It is enabled, and its `roles` include the role.
2. Its provider is `available` on the run's host, and `provider_models` lists the model and the
   thinking level.
3. `eligible_until IS NULL` (the plan renews) or `now < eligible_until`.
4. It is not excluded for this run, and no cooldown is active. A cooldown comes from a reported
   quota error or launch failure. It lasts until the failing window's reset, or 30 minutes if
   the reset is unknown.
5. Its long window and every gate window have a reading that is not stale (15 minutes), and each
   has `R_adj > reserve_pct`.

Routes that meet 1-4, have no recent quota error, but have a stale or missing meter (Muse today)
are **probe** routes. A probe route is listed after the eligible routes, marked "meter unknown".
It is never the main suggestion and never the automatic fallback. A probe lets the orchestrator
try it on purpose, so a meter that reports nothing before first use cannot exclude a plan
forever.

Fallbacks:

- **The launch fails** (unknown model or mode, provider unavailable): call `suggest --exclude`.
  The route goes into cooldown, and the next eligible route is suggested.
- **A quota error** (`RESOURCE_EXHAUSTED`, 429, usage limit): the same, with `--why`.
- **The plan ended** (`eligible_until` has passed): not eligible.
- **Fall back only to an eligible route.** There is no fixed default model. Opus is suggested
  only when `opus` itself passes 1-5. If Claude was excluded, exhausted or unavailable, it is not
  suggested.
- **No route is eligible:** the outcome is `wait`. The row stores `wait_until`: the earliest
  reset, cooldown end or fresh reading that could change the answer. The block lists each route
  with the reason it is out, and says: "No plan has established capacity. Wait until
  `<wait_until>`, then run `intelligence workflow suggest`. If that is more than 1 hour away,
  ask Zach." Waiting is one bounded foreground wait (the paseo-cli skill rule), not polling. The
  orchestrator may start a probe route only if Zach agrees or the step allows it.

### 5.6 Keep checkers on a different model from builders

The checker query reads the run's builders: `recorded_agents` with the same
`intelligence.run` and `role = builder`. It removes routes of the same `family`. If no other
family is eligible, it suggests the same family with a note, and a fresh agent is still required.
`agy-opus` counts as `anthropic`, so it never "diversifies" a Claude builder.

### 5.7 Claude logins (outside this repository)

Model choice cannot move work between the two Claude logins. Change `cswap-auto` from "switch at
90%" to balancing. For example: switch to the login with the higher pace when the gap is more
than 15 points, at most once an hour, and never while that login is below its reserve. The
`ai-subscription-limits` skill says where the unit and its thresholds are. This change has the
largest effect in slow weeks. If login 1 stays below 90% until its reset, login 2 goes a whole Ultra week without use.

### 5.8 Rollout

| Phase | Change | Done when |
| --- | --- | --- |
| 0 (no code, now) | Edit the saved Factory Task in the editor: delete the two-plan sentence. Give the five options as exact `--provider`/`--thinking` ids. Add "checker: a different family from the builder". Add a dated note: "until 2026-11-11 06:48 UTC prefer Codex; until the Google plan ends prefer Gemini Argon". Separately, change cswap-auto to balancing. | Next runs use Argon and Codex. Claude login 2 rises above 0%. |
| 1 (read-only) | Migrations 0029-0030 and the views. A "Plans" card on the home page that shows `strand` per route ("Codex: 59% left, resets in 12 h; about 52 points left over even at full pace"). Write each test first. `db.CheckContract` covers the new relations. | The SQL agrees with `cswap list`, `agy /usage` and the Codex meter. |
| 2 (advisory) | The suggestion block, `workflow_model_suggestions`, the `suggest` route and CLI, and labels on launches | Acceptance rate (labels compared with suggestions) and spread by route are readable in SQL. |
| 3 (default) | New instruction text. Saved definitions drop their lists. Optional `Step.models` (pin, allow, role) for steps that need one model. | At least 80% of sub-agents follow suggestions. No plan strands more than 20% of a week while others are short. |
| 4 (tune) | Use outcomes per route (send-backs per builder route, checker findings) to set the allowlist and the weights | Decisions use at least 2 weeks of data. |

### 5.9 Relation to the 2026-11-08 proposal

[`docs/proposals/2026-11-08-model-suggestion.md`](2026-11-08-model-suggestion.md) has
the same direction: advisory, usage-based, with recorded choices. This plan changes four things:

1. The ranking is a SQL view, so anyone can read and check it. In the earlier proposal it was a
   Go score.
2. The horizon includes the plan's end, so plans that are ending are spent first.
3. It reuses Paseo labels and `workflow_run_agents` for "what actually ran", instead of a new
   token table.
4. It treats the Claude login balance as a cswap problem.

The allocation penalty in 5.3 is a light form of the earlier proposal's leases: each suggestion
counts against its route for 60 minutes, under one lock per account. Hard leases with release
on finish, and token accounting, stay deferred until data shows they are needed.

## 6. What I could not check

- **Intelligence's own tables** (`usage_readings`, `workflow_runs`, saved definitions). `/api/sql`
  needs a signed-in account session, and I have no credential for one. I did not look for one.
  I read the saved definitions from the orchestrators' first prompts instead.
- **Other hosts.** The agent store and the Codex rollouts cover the workflow host only. The Codex
  and Claude meters are account-wide, so the peaks are correct, but agents counted on other hosts
  are missing. PDW shows few: 63 Opus sessions and 41 Argon sessions on the Mac host.
- **Muse:** `muse usage --json` returned `server_sent_no_usage`. I did not use the browser quota
  endpoint.
- **Plan end dates** for Google (Nov 13 or 14) and Muse (Nov 13) come from the skill. I did not
  check them against the billing pages.
- **Role** comes from title keywords: 11 workflow sub-agents fell into "other". The counts can be
  off by a few.
