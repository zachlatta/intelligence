-- Each git workspace's diff: what its work changed against its base branch (the merge base), with
-- uncommitted and untracked changes, as its host's Paseo daemon parses it. The host sends a
-- workspace's diff when it changed (hostclient.WorkspaceDiff); a diff goes when its workspace is
-- no longer listed by a host connected to its daemon.

CREATE TABLE workspace_diffs (
    host         text NOT NULL REFERENCES hosts (name) ON DELETE CASCADE,
    workspace_id text NOT NULL,
    directory    text NOT NULL DEFAULT '',
    error        text NOT NULL DEFAULT '',        -- why the daemon could not read it (not a git repository, ...)
    too_large    boolean NOT NULL DEFAULT false,  -- the daemon gave up on it whole
    file_count   integer NOT NULL DEFAULT 0,
    additions    integer NOT NULL DEFAULT 0,
    deletions    integer NOT NULL DEFAULT 0,
    -- [{"path","oldPath","isNew","isDeleted","additions","deletions","status","hunks":[{"oldStart","oldCount",
    -- "newStart","newCount","lines":[{"type","content"}]}]}], by path. status: ok, binary or too_large (no hunks).
    files        jsonb NOT NULL DEFAULT '[]',
    collected_at timestamptz NOT NULL,
    received_at  timestamptz NOT NULL,
    PRIMARY KEY (host, workspace_id)
);
COMMENT ON TABLE workspace_diffs IS 'each git workspace''s diff against its base, as its host last read it';

-- One row per changed file.
CREATE VIEW workspace_diff_files AS
SELECT d.host, d.workspace_id, f.position::int AS position,
       f.file ->> 'path' AS path,
       coalesce(f.file ->> 'oldPath', '') AS old_path,
       coalesce((f.file ->> 'isNew')::boolean, false) AS is_new,
       coalesce((f.file ->> 'isDeleted')::boolean, false) AS is_deleted,
       coalesce((f.file ->> 'additions')::int, 0) AS additions,
       coalesce((f.file ->> 'deletions')::int, 0) AS deletions,
       coalesce(f.file ->> 'status', 'ok') AS status,
       coalesce(f.file -> 'hunks', '[]') AS hunks
FROM workspace_diffs d
CROSS JOIN LATERAL jsonb_array_elements(d.files) WITH ORDINALITY AS f (file, position);
COMMENT ON VIEW workspace_diff_files IS 'each changed file of each workspace''s diff';

ALTER TABLE workspace_diffs ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_hosts ON workspace_diffs FOR SELECT
    USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));
ALTER VIEW workspace_diff_files SET (security_invoker = true);
