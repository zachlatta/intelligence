-- A project can exist without any listed workspace.
ALTER TABLE hosts ADD COLUMN projects jsonb NOT NULL DEFAULT '[]';

CREATE VIEW host_repositories WITH (security_invoker = true) AS
SELECT h.name AS host, p ->> 'directory' AS directory, p ->> 'name' AS project
FROM hosts h CROSS JOIN LATERAL jsonb_array_elements(h.projects) p
WHERE p ->> 'kind' = 'git' AND coalesce(p ->> 'directory', '') <> ''
UNION
SELECT host, project_root AS directory, project FROM workspaces
WHERE project_root <> '' AND kind <> 'directory';
