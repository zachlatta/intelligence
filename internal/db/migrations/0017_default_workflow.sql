-- Defaults belong to accounts; existing workflows keep their creation order until one is chosen.
ALTER TABLE workflows ADD COLUMN is_default boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX workflows_one_default_per_account ON workflows (account_id) WHERE is_default;
