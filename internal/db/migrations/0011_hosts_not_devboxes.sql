-- One word for the machines: a host, not a dev box (or a box). The tables, columns, views,
-- constraints, indexes and policies that said devbox or box say host. Renames keep each object's
-- grants, row-level security, policies and view options (security_invoker).

ALTER TABLE devboxes RENAME TO hosts;
ALTER TABLE devbox_keys RENAME TO host_keys;
ALTER TABLE devbox_enrollments RENAME TO host_enrollments;
ALTER VIEW devbox_status RENAME TO host_status;

-- Every box column is host, in tables and in the views that pass it through.
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT c.relname, c.relkind FROM pg_attribute a
             JOIN pg_class c ON c.oid = a.attrelid
             WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'v')
               AND a.attname = 'box' AND a.attnum > 0 AND NOT a.attisdropped
             ORDER BY c.relkind, c.relname LOOP
        EXECUTE format('ALTER %s %I RENAME COLUMN box TO host',
            CASE r.relkind WHEN 'v' THEN 'VIEW' ELSE 'TABLE' END, r.relname);
    END LOOP;
END $$;

-- Constraint names (a primary key's or unique constraint's renames its index too), indexes and policies.
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT c.relname, k.conname FROM pg_constraint k
             JOIN pg_class c ON c.oid = k.conrelid
             WHERE c.relnamespace = 'public'::regnamespace AND k.conname ~ 'devbox|box' LOOP
        EXECUTE format('ALTER TABLE %I RENAME CONSTRAINT %I TO %I', r.relname, r.conname,
            replace(replace(replace(r.conname, 'devboxes', 'hosts'), 'devbox', 'host'), 'box', 'host'));
    END LOOP;
    FOR r IN SELECT c.relname FROM pg_class c
             WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'i' AND c.relname ~ 'devbox|box' LOOP
        EXECUTE format('ALTER INDEX %I RENAME TO %I', r.relname,
            replace(replace(r.relname, 'devbox', 'host'), 'box', 'host'));
    END LOOP;
END $$;

-- The policies, made again (a renamed one keeps the old column name as an alias in its text).
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT c.relname FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
             WHERE c.relnamespace = 'public'::regnamespace AND p.polname = 'own_boxes' AND c.relname <> 'hosts' LOOP
        EXECUTE format('DROP POLICY own_boxes ON %I', r.relname);
        EXECUTE format('CREATE POLICY own_hosts ON %I FOR SELECT USING (host IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()))', r.relname);
    END LOOP;
END $$;
DROP POLICY own_boxes ON hosts;
CREATE POLICY own_hosts ON hosts FOR SELECT
    USING (name IN (SELECT k.host FROM host_keys k WHERE k.account_id = current_account_id()));

COMMENT ON COLUMN hosts.last_seen IS 'when the server last heard from the host: a report, or a ping on its stream';
COMMENT ON COLUMN hosts.paseo_error IS 'why the host is not connected to its daemon';
COMMENT ON COLUMN hosts.stream_id IS 'the host''s current stream to the server; null while it has none';
