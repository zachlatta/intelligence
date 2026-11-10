# Workflow submission and input trust

**Intelligence already has a run-submission API. Add scoped credentials and recorded input provenance before giving it to agents.** A verified request from Zach may start an allowed workflow. Email from Sarah or Christina must remain data, with no right to start a run or answer an approval.

> **What changes for you, if this proposal is built:** Your verified mail and task memos start the intake workflow. Everyone else's mail remains read-only data. An agent can start only the child workflows your task and policy authorize.

**Repair legacy intake early.** The investigation found a legacy dispatcher destination that needs migration to Intelligence. Reconcile deferred work and add replay protection before cutover. Detailed operational evidence is retained in the private investigation; this public proposal omits deployment settings and ledger data.

This is a design proposal, not an implemented control. Source review used commit `7d63df7a0e708a8fca242a68d85c2ded6ed64675`, on 2026-11-10. All citations were rechecked against `dc6141d501b4e978bf193f2fcbade0a025b0b99c`, the same day. No code, schema, service, or shared skill was changed. Live dispatcher evidence comes from its process environment, listening-port table, service journal, and read-only ledger inspection. Its installed script matches the inspected external source byte for byte. No private message content or deployment address is included here.

## 1. How submission works now

| Entry point | What it does | Evidence |
| --- | --- | --- |
| Workflows page task box | Posts `{workflowId, input}` to `/api/workflow-runs`. Image references become part of the input. Only task workflows appear in this picker. | `internal/web/workflows.html:161`, `internal/web/workflows.html:259`; `internal/web/workflows.go:573` |
| Workflow editor's Run dialog | Posts to the same API with the selected workflow and task text. | `internal/web/workflows.html:645` |
| Browser dictation | Fills those text boxes. It uses the same submission API and browser session. There is no separate voice identity in the resulting run. | `internal/web/workflows.html:285`, `internal/web/workflows.html:662`; `internal/web/dictation.go:38` |
| JSON client | Can call the same run API after signing in through `/api/session` and keeping its cookie. There is no workflow-submit bearer credential. | `internal/web/server.go:117`, `internal/web/server.go:182`; `internal/web/auth.go:29` |
| `intelligence workflow` CLI | Moves an existing run: `project`, `next`, `back`, `skip`, `ask`, `done`, `fail`. It does not submit a task or spawn a workflow. | `internal/workflow/cli.go:19`, `internal/workflow/cli.go:49`, `internal/workflow/cli.go:111` |
| Intelligence Gmail intake | Reads each connected mailbox every 15 seconds. Stores each message and its decision. Matches email workflow filters, then calls `StartRunTx` directly. | `internal/email/service.go:35`, `internal/email/service.go:283`, `internal/email/service.go:328`, `internal/email/service.go:425` |
| Email in a run's thread | May add a message, answer a waiting run, or start another run. See the reply risk below. | `internal/email/service.go:374` |
| Paseo schedules | Run a prompt in a new or existing agent. Intelligence reports schedule runs and sends schedule commands. This is distinct from creating `workflow_runs`. No schedule trigger exists in the workflow definition. | `internal/hostclient/schedules.go:17`, `internal/hostclient/schedules.go:41`; `internal/workflow/definition.go:62` |
| Direct agent/workspace creation | The commands API can start an agent without a workflow. That alone creates no workflow run. | `internal/web/stream.go:240`; `internal/hostclient/commands.go:242` |
| Old factory board, Scouts and ideas | Separate application and API. Its `/api/new` created board cards, not this app's workflow rows. The current `agent-factory` skill marks the deployment retired. | External evidence below |
| Phone voice memos through zrl-bot | Separate dispatcher classifies Apple Voice Memos and Google Recorder transcripts, files task mail, then submits a factory card. The inspected source still targets the old factory API. It is not an Intelligence voice-memo adapter. | External evidence below |

The workflow trigger values are `task`, `manual`, and `email` (`internal/workflow/definition.go:62`). The trigger is not an API authorization boundary. `StartRunTx` does not require `task` or `manual` when an account submits a workflow ID (`internal/workflow/engine.go:343`). A client can therefore manually start an owned email workflow too.

### From request to running orchestrator

1. The web handler gets the account from its session. It calls `Engine.StartRun` (`internal/web/workflows.go:173`). Gmail calls the transaction form of that same operation.
2. The engine rejects empty input. It selects the account's undeleted workflow and current definition version. It checks ownership of the definition's host (`internal/workflow/engine.go:343`).
3. In one transaction, it inserts `workflow_runs` and a `started` event. The run pins the definition version (`internal/workflow/engine.go:374`; `internal/db/migrations/0015_workflows.sql:33`).
4. For a fixed project, it queues a workspace and orchestrator. For a matched project, it first queues a project-finder agent. The finder's `project` move chooses the repository, then starts the orchestrator (`internal/workflow/engine.go:386`, `internal/workflow/engine.go:421`, `internal/workflow/engine.go:539`).
5. `fleet.EnqueueTx` inserts a durable command and emits a Postgres notification. The server sends it over the host's outbound WebSocket stream (`internal/fleet/commands.go:102`; `internal/web/stream.go:185`).
6. The host calls Paseo to create the workspace and agent. Command IDs make these creations safe to redeliver (`internal/hostclient/commands.go:242`, `internal/hostclient/commands.go:383`).
7. The host reports the result. `CommandUpdated` records the workspace and orchestrator IDs and moves the run to `running`. A failed command records a failed run (`internal/web/stream.go:154`; `internal/workflow/engine.go:456`).

The API's 201 means the run and launch command were recorded. It does not mean Paseo has started the orchestrator. Read status through SQL.

## 2. The existing API and its gaps

```http
POST /api/workflow-runs
Content-Type: application/json
Cookie: intelligence_session=<session>

{"workflowId":"<workflow UUID>","input":"Research the proposed change."}
```

```http
HTTP/1.1 201 Created
Location: /workflow-runs/<run UUID>
Content-Type: application/json

{"id":"<run UUID>"}
```

The handler is at `internal/web/workflows.go:164`. JSON is limited to 1 MiB and decoded strictly (`internal/web/workflows.go:45`). The account must own the workflow and host. Missing workflows give 404; invalid input gives 400 (`internal/web/workflows.go:61`; `internal/workflow/engine.go:343`).

The write wrapper requires a signed-in account, JSON, and a same-origin browser request. A command-line client without browser origin headers is allowed, but still needs the cookie (`internal/web/auth.go:76`, `internal/web/auth.go:114`). The cookie is Secure, HttpOnly, and SameSite=Strict (`internal/web/auth.go:125`). These controls protect browser requests. They do not express input trust.

The API has no submission idempotency key, source identity, parent run, trust decision, per-workflow caller grant, or child budget. A repeated POST can create a second run. Run creation has no caller-supplied version pin; the engine takes the current version. There is no dedicated submit CLI.

The moves API accepts a host key and checks that the run is on that host (`internal/web/workflows.go:216`, `internal/web/workflows.go:230`; `internal/workflow/engine.go:520`). The CLI reads the installed host credential (`internal/workflow/cli.go:137`). This is host scope, not agent or run scope. Do not reuse it as permission to submit arbitrary workflows.

### Existing email checks do not enforce the requested rule

Email authentication and workflow-start authorization are separate checks. The current intake needs a consistent authorization policy across all message paths (`internal/email/message.go`; `internal/workflow/email.go`; `internal/email/service.go`). Authentication alone must not grant permission to start work.

An owner address plus verification sets `fromZach`. Other email gets a prompt warning to treat it as information (`internal/email/service.go:329`; `internal/workflow/email.go:214`). This warning is useful context, not a permission check.

Replies need the same authorization review as initial messages. An initial sender filter is not a complete owner-only policy for a conversation. Enforce the intended rights on every transition, and distinguish data messages from task instructions and approval answers. Exact bypass conditions are omitted pending remediation.

### What zrl-bot's allowlist actually means

`ALLOWED_RECIPIENTS` controls where the email MCP server may **send** mail. It does not grant incoming senders task authority. Outbound recipient permission must remain separate from workflow intake permission.

The legacy dispatcher has a separate incoming-sender policy. Preserve its authentication requirements during migration and review them against the proposed owner-only policy. External source locations and deployment details are omitted from this public report.

Legacy voice intake classifies recordings as tasks or logs. Source selection and classification do not prove a speaker's identity. Migration needs authenticated source evidence and an explicit task-capture policy.

Legacy instructions and the current workflow system need reconciliation. Verify the running destination and pending work at cutover; do not assume that older instructions describe a working bridge. No live configuration was changed.

## 3. Proposed submission contract

Keep `POST /api/workflow-runs`. Add a separate bearer-authentication branch with narrowly defined rights. Keep the browser's cookie and origin checks unchanged. Refuse ambiguous cookie-plus-bearer requests. Host keys must not qualify as submit tokens.

Trusted adapters register input through `POST /api/workflow-inputs`. They submit a stable source event ID and reference. The server verifies the connector's evidence or retrieves it through a trusted connector. An agent cannot register an input as owner-authored. A broad adapter credential must stay outside the agent's process.

Example submission after the adapter has established provenance:

```http
POST /api/workflow-runs
Authorization: Bearer <scoped capability>
Idempotency-Key: <stable request key>
Content-Type: application/json

{
  "workflowId": "<allowed workflow UUID>",
  "inputId": "<verified input UUID>",
  "input": "Research the proposed change."
}
```

```json
{"id":"<run UUID>","status":"starting","parentRunId":null,"replayed":false}
```

Return 201 for creation and 200 for an identical retry. Return the same run ID. Bind the key to the authenticated principal and request hash. Return 409 if the same key has different content. Reject invalid/expired credentials with 401, disallowed input or workflow with 403, unknown owned targets with 404, and exhausted rate budgets with 429. A run state conflict gives 409. No client may assert `trusted`, `owner`, `parentRunId`, `rootRunId`, or account ID.

For an agent, add:

```text
intelligence workflow spawn --workflow <id> --input-id <id> --input task.md --idempotency-key <key>
```

The CLI calls the same API. The capability fixes the parent run and allowed targets. The response records the child run. The child starts through the existing engine and command stream. The parent does not get credentials to move, cancel, or reply to arbitrary children. Child status is SQL-readable through a separately scoped reader or a host broker that runs fixed, authorized SQL. Do not give agents an account-wide session merely to query status.

### Proposed migrations

Add additive migrations under `internal/db/migrations/`. This is a schema sketch, not executable SQL.

| Relation or change | Fields and constraints |
| --- | --- |
| `workflow_principals` | `id`, `account_id`, kind (`owner`, `connector`, `device`, `schedule`, `run`), enabled, timestamps, revoked time. Keep private identity mappings account-scoped. |
| `workflow_inputs` | `id`, `account_id`, source kind, connector principal, stable external event ID, author principal, received time, content digest, verification facts, trust class, verifier/policy version. Unique `(account_id, connector_id, external_event_id)`. Immutable after verification; corrections append a new record. |
| `workflow_input_links` | Account, derived input ID, source input ID, relation (`reply`, `quote`, `attachment`, `transcript`). Preserve mixed-source ancestry. |
| `workflow_submit_credentials` | Credential ID, account, principal, token hash, parent/root run if any, source input, exact rights, expiry, revocation, budget. Store no usable token. |
| `workflow_submission_rules` | Account, target workflow, allowed source principals/trust classes, allowed caller workflows or connectors, child permission, limits, policy version, enabled. Deny by default. |
| Extend `workflow_runs` | `input_id`, `caller_principal_id`, `credential_id`, `parent_run_id`, `root_run_id`, `depth`, submission-policy snapshot. Existing runs become `legacy_unknown` with no child authority. |
| `workflow_submission_attempts` | Account, caller, input, target, parent, idempotency key/hash, decision, reason code, policy version, timestamp, resulting run ID. Unique key per caller. Keep refusals too. |

Use composite foreign keys with `account_id` to prevent cross-account input, parent, credential, and workflow links. Use check constraints for nonnegative limits and depth. Keep root budgets in durable rows and lock them when creating a child. Atomically authorize, reserve budget, record the attempt, create the run, and enqueue its command. A denied attempt must commit its audit record without creating a run.

All these tables live in `public`. Enable account row-level security and grant the existing read-only role SELECT. Any summary view must use `security_invoker`. Reuse the patterns in `internal/db/migrations/0015_workflows.sql:74` and `internal/db/migrations/0018_email_triggers.sql:53`. Preserve `db.CheckContract` and its tests (`internal/db/db.go:116`). Hash credentials so SQL access cannot recover them. Return a newly issued secret once; never put it in prompts, command JSON, events, transcripts, or SQL. Keep mutable policy, budgets, revocation, and audit state in Postgres.

Policy changes and credential creation/revocation use owner-session JSON writes under `/api/`. Proposed routes: `PUT /api/workflows/{id}/submission-policy`, `POST /api/workflow-submit-credentials`, and `DELETE /api/workflow-submit-credentials/{id}`. Policy reads, input reads, audit, and run trees use `/api/sql`; add no new read endpoint.

## 4. Trust and access control

### Establish identity before the model sees the task

For email, fetch the message from the configured authenticated mailbox API. Parse the trusted receiver's authentication results with exact auth-server and domain boundaries. Reject duplicate or malformed From fields. Do not trust an `Authentication-Results` string supplied in a submission body.

Require the approved owner's exact address and aligned DMARC authentication. Record SPF and DKIM verdicts and their authenticated domains. DMARC requires a passing aligned SPF or DKIM identity; unrelated pass results do not qualify. For the initial zrl-bot migration, retain its additional exact-envelope SPF and Return-Path requirements. Treat failures as non-authoritative input. Revisit legitimate forwarded mail separately. Domain authentication does not prove which human typed the message; mailbox access and exact-address policy remain part of the trust assumption. See [RFC 9989](https://www.rfc-editor.org/rfc/rfc9989.html).

For voice memos, bind the connector to Zach's enrolled device/account and a task-capture channel. Store the original recording ID, source account/device principal, recording digest, and transcript digest. Authenticate uploads with a revocable device or ingestion credential. Replay protection uses the stable recording ID. A transcript saying “I am Zach” grants nothing. A task/log classifier grants nothing. Recordings of meetings, calls, forwarded audio, or imported files are data unless Zach approves a specific task. An enrolled device proves the capture channel; it does not prove the speaker. Use an explicit task action or confirmation where that distinction matters.

### Enforce rights outside the agent

Effective child rights are the intersection of verified input authority, the caller's capability, the parent's policy, and the target workflow's allowlist. Both source and target must permit the edge. Default to no child rights. Unknown provenance fails closed. A schedule needs its own owner-approved principal and target grant; it does not inherit owner authority from its prompt.

Sarah's or Christina's mail may be stored, searched, or shown to a data-processing agent. It must not directly start a workflow, start a follow-up, answer a waiting approval, or grant child rights. Apply this check before every email branch in `Service.route` and `Service.reply`, including terminal-run replies. Outbound recipient permission remains unchanged.

The hard case is a trusted run reading untrusted mail. This is the **confused deputy** risk: malicious text tries to use the agent's existing authority. A root trust flag and a warning in the prompt cannot prevent it. Do not put general spawning credentials in an agent that reads an unrestricted inbox.

Use a broker outside the model process. Pre-authorize bounded child actions from Zach's task, with exact targets and constrained arguments. The agent may request those actions but cannot expand their authority. For open-ended decisions after reading third-party text, require a fresh owner approval of the target and exact task. Bind approval to a content digest and one submission. Revoke old grants on cancellation or policy change. If a third-party message would otherwise be steered into a privileged agent, send it to a separate agent with no spawn rights instead. Do not rely on the model to report that it read untrusted data.

Isolation is a prerequisite. Token scope alone cannot secure a worker that has other powerful tools. Run untrusted-input workers under separate OS identities or containers. Deny access to host keys, owner cookies, the privileged broker, other agents' credentials, and direct agent creation. Limit tools and network egress, including file delivery (`internal/workflow/definition.go:222`, `internal/workflow/definition.go:234`, `internal/workflow/definition.go:239`). Enforce private-data restrictions outside prompts. Until this is enforced, describe the system as cooperative automation, not a hard security boundary against prompt injection.

### Bound children and preserve the audit trail

Start with these proposed limits: depth 1, at most 3 children per parent, at most 3 descendants per root, and at most 2 live runs per root. Reject a workflow ID already in the ancestor chain. Allow only explicit source-to-target edges. Count attempts against a separate rate limit so repeated refusals cannot exhaust the service. Use the same idempotency key on network retries. Lock budget rows so concurrent requests cannot exceed limits.

Child capabilities may only shrink. They cannot acquire owner or connector rights. At depth 1, children get no spawn rights. Expire grants within 15 minutes; renew only through the broker while the parent and policy remain valid. Cancellation revokes new starts across the tree. Record existing-child cancellation as a separate explicit policy; first release should cancel queued descendants and request cancellation of running ones.

Audit every authenticated request, including denials. Keep caller, verified author, source input, source digest, parent/root, target/version, policy version, limits used, outcome, and reason. Preserve the whole source chain through replies and transcription. Do not log tokens or raw authorization headers. Rate-limit and aggregate unauthenticated abuse separately. SQL should answer: “What input authorized this child, which check allowed it, and what else did this root start?”

## 5. Recommended phases and decisions

1. **Close current intake gaps first.** Make repairing the broken zrl-bot submission route an early action, with backlog reconciliation and replay protection before replay. Do not restore the retired board or bypass trust checks for a quick recovery. Add failing tests for verified non-owner replies, terminal threads, filters without senders, spoofed headers, and owner approvals. Enforce a separate workflow-start permission on every email branch. A workflow may explicitly allow verified non-owner replies as data in a live run, while still refusing them as new runs, terminal-run follow-ups, waiting approvals, or sources of child authority. Until isolated data-processing workers are available, store those replies for review instead of steering them into an agent with spawn rights. Test both this opt-in data path and the default refusal. Mark existing provenance unknown. No agent submit token yet.
2. **Add trusted submissions.** Add provenance, durable idempotency, policy, audit, and connector credentials. Pilot one explicit workflow for Zach-only verified email. Test rollback, retries, deletion, revocation, and cross-account attempts. Preserve the current browser path.
3. **Move voice intake.** Recheck dispatcher state at cutover. Reconcile pending email and voice records. Deduplicate old and new targets. Use one active submission destination per source. Add authenticated device/source evidence; do not copy a self-sent email's identity into a voice trust decision.
4. **Pilot child runs.** Add parent links and broker-issued capabilities after worker isolation. Use the fixed limits above. Test injection through a third-party email, forged provenance, token theft across workers, cycles, concurrent budgets, parent cancellation, and changed workflow policy/version. The expected result is refusal outside a pre-authorized action.
5. **Expand only from audit evidence.** Add explicit schedule grants and more workflow edges. Revisit depth and limits after reviewing real run trees and failures.

Open decisions for Zach, with recommendations:

- Should Sarah and Christina be able to request any automated work? Recommend storing their mail as data only, with no direct or child run-start right.
- Which workflows may owner mail and task memos start? Recommend one explicit intake workflow first, not every workflow in the account.
- May a trusted run choose arbitrary child tasks after reading third-party content? Recommend only pre-authorized bounded actions; otherwise approve the exact child task.
- Do all device recordings count as tasks? Recommend only the task-capture channel. Keep meetings and imported audio as data.
- Should canceling a parent stop its children? Recommend yes for the first release, with each cancellation outcome recorded.

## Verification and proposed external follow-up

This step delivers documentation only. No API implementation or new tests were needed. Before committing, run the repository's full `make test`, including hygiene and the read-only SQL contract against a throwaway database. The handoff records its result.

For a later authorized step: repair the confirmed broken dispatcher route through the trusted submission path; recheck and reconcile its ledger before replay; update stale `zrl-bot-email` and dispatcher instructions after the actual migration is verified. Do not restart the retired board. Do not change `ALLOWED_RECIPIENTS` as part of workflow intake. No external configuration change is required to land this report.
