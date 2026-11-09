-- Each host's provider catalog: the models (with the thinking levels each takes) and permission
-- modes an agent of each provider can start with, as the host's Paseo daemon last listed them.
-- They travel in hosts.providers ([{"name","available","error","label","defaultMode","models":
-- [{"id","label","description","default","thinking","defaultThinking"}],"modes":[{"id","label",
-- "description"}]}]); these views read them one row each. Position is the daemon's order.

CREATE VIEW provider_models AS
SELECT h.name AS host,
       p ->> 'name' AS provider,
       m.position::int AS position,
       m.model ->> 'id' AS id,
       coalesce(m.model ->> 'label', '') AS label,
       coalesce(m.model ->> 'description', '') AS description,
       coalesce((m.model ->> 'default')::boolean, false) AS is_default,
       coalesce(ARRAY(SELECT jsonb_array_elements_text(m.model -> 'thinking')), '{}') AS thinking,
       coalesce(m.model ->> 'defaultThinking', '') AS default_thinking
FROM hosts h
CROSS JOIN LATERAL jsonb_array_elements(h.providers) p
CROSS JOIN LATERAL jsonb_array_elements(coalesce(p -> 'models', '[]')) WITH ORDINALITY AS m (model, position);
COMMENT ON VIEW provider_models IS 'each host''s providers'' models, as its Paseo daemon last listed them';

CREATE VIEW provider_modes AS
SELECT h.name AS host,
       p ->> 'name' AS provider,
       m.position::int AS position,
       m.mode ->> 'id' AS id,
       coalesce(m.mode ->> 'label', '') AS label,
       coalesce(m.mode ->> 'description', '') AS description,
       m.mode ->> 'id' = p ->> 'defaultMode' AS is_default
FROM hosts h
CROSS JOIN LATERAL jsonb_array_elements(h.providers) p
CROSS JOIN LATERAL jsonb_array_elements(coalesce(p -> 'modes', '[]')) WITH ORDINALITY AS m (mode, position);
COMMENT ON VIEW provider_modes IS 'each host''s providers'' permission modes, as its Paseo daemon last listed them';

ALTER VIEW provider_models SET (security_invoker = true);
ALTER VIEW provider_modes SET (security_invoker = true);
