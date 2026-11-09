-- Workflows: a trigger's input, a project found for it, and an orchestrator agent in that
-- project's worktree that is handed the workflow's steps one at a time (internal/workflow).

-- A workflow, named; its definition is in workflow_versions, one row per save.
CREATE TABLE workflows (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    version     integer NOT NULL, -- the latest
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE INDEX workflows_account ON workflows (account_id);

-- Every saved version of every workflow. A run keeps the version it started with.
-- definition: {"trigger", "project": {"mode", ...}, "orchestrator": {...}, "steps": [{"id", "kind", "title", "instructions"}]}
CREATE TABLE workflow_versions (
    workflow_id uuid NOT NULL REFERENCES workflows ON DELETE CASCADE,
    version     integer NOT NULL,
    account_id  bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    definition  jsonb NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (workflow_id, version)
);

-- One run of a workflow version on a host.
--   finding_project: an agent is choosing the repository (or the definition names one)
--   starting:        the worktree and the orchestrator are being made
--   running:         the orchestrator is on step `step` (0-based)
--   waiting:         the orchestrator asked Zach (`note`, Markdown) and waits for his reply
--   succeeded, failed (`error`), canceled
CREATE TABLE workflow_runs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id       bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    workflow_id      uuid NOT NULL,
    version          integer NOT NULL,
    host              text NOT NULL,
    title            text NOT NULL,
    input            text NOT NULL,
    status           text NOT NULL CHECK (status IN ('finding_project', 'starting', 'running', 'waiting', 'succeeded', 'failed', 'canceled')),
    step             integer NOT NULL DEFAULT 0,
    repo             text NOT NULL DEFAULT '', -- the repository's main checkout on the host; '' for none
    branch           text NOT NULL DEFAULT '', -- the worktree's branch
    workspace_id     text NOT NULL DEFAULT '',
    project_agent_id text NOT NULL DEFAULT '', -- the agent that found the project
    orchestrator_id  text NOT NULL DEFAULT '',
    pending_command  uuid,                     -- the command the run waits on: making the finder, or the worktree and orchestrator
    project_command  uuid,                     -- the command that made the finder
    note             text NOT NULL DEFAULT '', -- the orchestrator's question while waiting
    error            text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    finished_at      timestamptz,
    FOREIGN KEY (workflow_id, version) REFERENCES workflow_versions (workflow_id, version) ON DELETE CASCADE
);
CREATE INDEX workflow_runs_account ON workflow_runs (account_id, created_at DESC);
CREATE INDEX workflow_runs_pending ON workflow_runs (pending_command) WHERE pending_command IS NOT NULL;

-- What happened in each run, in order: started, project, workspace, next, back, skip, ask, reply,
-- done, fail, cancel, error. step is the step it happened in, to_step where it went.
CREATE TABLE workflow_run_events (
    id         bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    run_id     uuid NOT NULL REFERENCES workflow_runs ON DELETE CASCADE,
    account_id bigint NOT NULL REFERENCES accounts ON DELETE CASCADE,
    at         timestamptz NOT NULL,
    kind       text NOT NULL,
    step       integer,
    to_step    integer,
    text       text NOT NULL DEFAULT ''
);
CREATE INDEX workflow_run_events_run ON workflow_run_events (run_id, id);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['workflows', 'workflow_versions', 'workflow_runs', 'workflow_run_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY own_rows ON %I FOR SELECT USING (account_id = current_account_id())', t);
    END LOOP;
END $$;
