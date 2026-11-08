-- Agents' transcripts: their projected Paseo timelines, synced while the agent is loaded. A
-- transcript belongs to its box rather than to the agents row, so a box that loses its daemon for
-- a while keeps them; it goes when its agent is gone from a connected box's report (archived).
CREATE TABLE transcripts (
    box       text NOT NULL REFERENCES devboxes (name) ON DELETE CASCADE,
    agent_id  text NOT NULL,
    epoch     text NOT NULL, -- seqs are comparable within one epoch; a new one replaces the transcript
    synced_at timestamptz NOT NULL,
    PRIMARY KEY (box, agent_id)
);

-- One projected timeline entry. item is the daemon's item whole; the other columns are read from it.
CREATE TABLE transcript_entries (
    box       text NOT NULL,
    agent_id  text NOT NULL,
    seq_start integer NOT NULL,
    seq_end   integer NOT NULL,
    turn_id   text NOT NULL DEFAULT '',
    provider  text NOT NULL DEFAULT '',
    at        timestamptz,
    item      jsonb NOT NULL,
    kind      text GENERATED ALWAYS AS (coalesce(item->>'type', '')) STORED,  -- user_message, assistant_message, reasoning, tool_call, ...
    tool      text GENERATED ALWAYS AS (CASE WHEN item->>'type' = 'tool_call' THEN coalesce(item->>'name', '') ELSE '' END) STORED,
    status    text GENERATED ALWAYS AS (CASE WHEN item->>'type' = 'tool_call' THEN coalesce(item->>'status', '') ELSE '' END) STORED,
    text      text GENERATED ALWAYS AS (CASE WHEN jsonb_typeof(item->'text') = 'string' THEN item->>'text' ELSE '' END) STORED,
    PRIMARY KEY (box, agent_id, seq_start),
    FOREIGN KEY (box, agent_id) REFERENCES transcripts (box, agent_id) ON DELETE CASCADE
);
