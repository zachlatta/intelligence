-- Subscription plans: every login's quota windows (how much is used, when each resets) over time.
-- Each dev box reads every provider's own meter every 5 minutes (internal/usage) and sends it up
-- its stream. A login is (provider, account): its email for Claude (every cswap login) and Codex,
-- "default" where a tool has one login. The same login read from two boxes is one login.

CREATE TABLE usage_accounts (
    provider   text NOT NULL,             -- claude, codex, muse, antigravity
    account    text NOT NULL,
    label      text NOT NULL DEFAULT '',  -- what to show: the email, or the tool's name
    plan       text NOT NULL DEFAULT '',  -- plan or tier, where the provider says
    slot       integer,                   -- cswap's number for it on the box that last read it; can change
    first_seen timestamptz NOT NULL,
    last_seen  timestamptz NOT NULL,      -- the newest reading of it, failed or not
    PRIMARY KEY (provider, account)
);

-- The history, as runs: a row is one window holding one value from first_observed to
-- last_observed (seen `observations` times). A new row starts only when the value or the reset
-- changes, so the history is exact and small. A window still at 0% whose reset keeps moving (a
-- rolling window not yet started) stays one run. A reading older than the newest is ignored.
CREATE TABLE usage_readings (
    provider       text NOT NULL,
    account        text NOT NULL,
    window_id      text NOT NULL,          -- 7d, 5h, or a pool's: fable/7d, gemini/5h, ...
    window_seconds integer,                -- its length; null unknown
    used_pct       double precision NOT NULL,
    resets_at      timestamptz,            -- to the minute; null unknown
    first_observed timestamptz NOT NULL,
    last_observed  timestamptz NOT NULL,
    observations   integer NOT NULL DEFAULT 1,
    PRIMARY KEY (provider, account, window_id, first_observed),
    FOREIGN KEY (provider, account) REFERENCES usage_accounts ON DELETE CASCADE
);

-- Which login each box's tool was using, as runs: cswap switches Claude logins.
CREATE TABLE usage_active (
    box      text NOT NULL,
    provider text NOT NULL,
    account  text NOT NULL,
    since    timestamptz NOT NULL,
    until    timestamptz NOT NULL,         -- last seen active
    PRIMARY KEY (box, provider, since),
    FOREIGN KEY (provider, account) REFERENCES usage_accounts ON DELETE CASCADE
);

-- Each box's latest read of each provider (account '') and of each login, and whether it worked.
-- A failed read stores no reading: old values stay as they were, with their age.
CREATE TABLE usage_sources (
    box         text NOT NULL,
    provider    text NOT NULL,
    account     text NOT NULL DEFAULT '',
    ok          boolean NOT NULL,
    error       text NOT NULL DEFAULT '',
    note        text NOT NULL DEFAULT '',  -- a caveat on the reading: a fallback, no usage yet
    observed_at timestamptz,
    received_at timestamptz NOT NULL,
    PRIMARY KEY (box, provider, account)
);

-- Each login's latest value of each window. remaining_pct is null once the window has reset
-- (there is no reading of the new one yet: never assume it is full) and while stale (no reading
-- in 15 minutes) it is kept but flagged.
CREATE VIEW usage_current AS
SELECT DISTINCT ON (r.provider, r.account, r.window_id)
       r.provider, r.account, a.label, a.plan, a.slot, r.window_id, r.window_seconds, r.used_pct,
       CASE WHEN r.resets_at IS NOT NULL AND r.resets_at <= now() THEN NULL ELSE 100 - r.used_pct END AS remaining_pct,
       r.resets_at,
       r.resets_at IS NOT NULL AND r.resets_at <= now() AS elapsed,
       r.last_observed,
       r.last_observed < now() - interval '15 minutes' AS stale
FROM usage_readings r
JOIN usage_accounts a USING (provider, account)
ORDER BY r.provider, r.account, r.window_id, r.first_observed DESC;

-- How each current window is spending against the clock: pace = % left minus % of the window
-- still to run. Positive is under-spending (quota will be left at the reset), negative is on
-- course to run out first. Only windows with a known length and reset, not yet elapsed.
CREATE VIEW usage_pace AS
SELECT c.*,
       100 * extract(epoch FROM c.resets_at - now()) / c.window_seconds AS window_left_pct,
       c.remaining_pct - 100 * extract(epoch FROM c.resets_at - now()) / c.window_seconds AS pace
FROM usage_current c
WHERE c.window_seconds > 0 AND c.resets_at IS NOT NULL AND NOT c.elapsed;

-- One row per window cycle (a login's window up to one reset): the most of it used, and when it
-- was seen. For a week's window, how much of each week each login used.
CREATE VIEW usage_cycles AS
SELECT provider, account, window_id, resets_at,
       max(used_pct)       AS peak_used_pct,
       min(first_observed) AS first_observed,
       max(last_observed)  AS last_observed
FROM usage_readings
WHERE resets_at IS NOT NULL
GROUP BY provider, account, window_id, resets_at;

-- Per provider: how many logins, and how many weeks of quota are left across them (the sum of
-- each login's weekly % left, from current, unstale readings).
CREATE VIEW usage_totals AS
SELECT a.provider,
       count(DISTINCT a.account)::int AS logins,
       count(DISTINCT c.account) FILTER (WHERE c.remaining_pct IS NOT NULL AND NOT c.stale)::int AS logins_read,
       coalesce(sum(c.remaining_pct) FILTER (WHERE NOT c.stale), 0) / 100 AS weeks_left
FROM usage_accounts a
LEFT JOIN usage_current c ON c.provider = a.provider AND c.account = a.account AND c.window_id = '7d'
GROUP BY a.provider;
