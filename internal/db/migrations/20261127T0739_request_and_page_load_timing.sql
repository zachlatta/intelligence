-- Request and page-load timing (internal/timing).
--
-- http_requests: one row per HTTP request the server answered: its route pattern (the mux's,
-- never the raw path), method, status, how long it took and when it started. No query string,
-- header, cookie, token, body, IP or user agent is stored. account_id is the signed-in account
-- (session, host key, workflow API key or MCP token), NULL when no one was; requests are written in batches
-- after they are answered, so an account deleted meanwhile leaves its id without a foreign key.
--
-- page_loads: each signed-in page's own navigation timing, reported by the browser
-- (POST /api/page-loads): its route pattern, navigation type, and time to first byte, to
-- DOMContentLoaded, to the load event and in total, in milliseconds.
--
-- Rows older than 30 days are pruned (timing.Retention).

CREATE TABLE http_requests (
    id          bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    at          timestamptz NOT NULL,
    account_id  bigint,
    method      text NOT NULL,
    route       text NOT NULL,
    status      integer NOT NULL,
    duration_ms double precision NOT NULL CHECK (duration_ms >= 0)
);
COMMENT ON TABLE http_requests IS 'every HTTP request the server answered: route pattern, method, status and duration; kept 30 days';

CREATE INDEX http_requests_at ON http_requests (at);
CREATE INDEX http_requests_route_at ON http_requests (route, at);

ALTER TABLE http_requests ENABLE ROW LEVEL SECURITY;
-- an account reads its own requests and those no one was signed in for (assets, sign-in, host
-- streams), which carry only a route pattern and timing
CREATE POLICY own_or_anonymous ON http_requests FOR SELECT
    USING (account_id = current_account_id() OR account_id IS NULL);

CREATE TABLE page_loads (
    id                    bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    at                    timestamptz NOT NULL,
    account_id            bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    route                 text NOT NULL,
    navigation_type       text NOT NULL,
    ttfb_ms               double precision NOT NULL CHECK (ttfb_ms >= 0),
    dom_content_loaded_ms double precision NOT NULL CHECK (dom_content_loaded_ms >= 0),
    load_ms               double precision NOT NULL CHECK (load_ms >= 0),
    duration_ms           double precision NOT NULL CHECK (duration_ms >= 0)
);
COMMENT ON TABLE page_loads IS 'each signed-in page load''s navigation timing, from the browser; kept 30 days';

CREATE INDEX page_loads_at ON page_loads (at);
CREATE INDEX page_loads_account_at ON page_loads (account_id, at);

ALTER TABLE page_loads ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON page_loads FOR SELECT
    USING (account_id = current_account_id());
