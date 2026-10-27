ALTER TABLE template_definitions
    ADD COLUMN network_policy jsonb,
    ADD COLUMN prepare_network_policy jsonb;

-- Preserve existing network behavior, including preparation when enabled.
UPDATE template_definitions
SET network_policy = '{"kind":"ordinary"}'::jsonb,
    prepare_network_policy = CASE WHEN prepare
        THEN '{"kind":"ordinary"}'::jsonb
        ELSE '{"kind":"block"}'::jsonb
    END;

ALTER TABLE template_definitions
    ALTER COLUMN network_policy SET NOT NULL,
    ALTER COLUMN prepare_network_policy SET NOT NULL,
    ADD CONSTRAINT template_definitions_network_policy_check CHECK ((
        network_policy IN ('{"kind":"ordinary"}'::jsonb, '{"kind":"block"}'::jsonb)
        OR (jsonb_typeof(network_policy) = 'object'
            AND network_policy->>'kind' = 'allow'
            AND jsonb_typeof(network_policy->'domains') = 'array'
            AND network_policy->'domains' <> '[]'::jsonb
            AND network_policy - ARRAY['kind', 'domains'] = '{}'::jsonb)
    ) IS TRUE),
    ADD CONSTRAINT template_definitions_prepare_network_policy_check CHECK ((
        prepare_network_policy IN ('{"kind":"ordinary"}'::jsonb, '{"kind":"block"}'::jsonb)
        OR (jsonb_typeof(prepare_network_policy) = 'object'
            AND prepare_network_policy->>'kind' = 'allow'
            AND jsonb_typeof(prepare_network_policy->'domains') = 'array'
            AND prepare_network_policy->'domains' <> '[]'::jsonb
            AND prepare_network_policy - ARRAY['kind', 'domains'] = '{}'::jsonb)
    ) IS TRUE),
    ADD CONSTRAINT template_definitions_seed_network_policy_check
        CHECK (prepare OR prepare_network_policy = '{"kind":"block"}'::jsonb);

ALTER TABLE agent_definitions
    ADD COLUMN runtime_mcp_policy jsonb;

UPDATE agent_definitions
SET runtime_mcp_policy = '{"kind":"unrestricted"}'::jsonb;

ALTER TABLE agent_definitions
    ALTER COLUMN runtime_mcp_policy SET NOT NULL,
    ADD CONSTRAINT agent_definitions_runtime_mcp_policy_check CHECK ((
        runtime_mcp_policy = '{"kind":"unrestricted"}'::jsonb
        OR (jsonb_typeof(runtime_mcp_policy) = 'object'
            AND runtime_mcp_policy->>'kind' = 'allow'
            AND jsonb_typeof(runtime_mcp_policy->'rules') = 'array'
            AND runtime_mcp_policy - ARRAY['kind', 'rules'] = '{}'::jsonb)
    ) IS TRUE);

CREATE TABLE session_mcp_scopes (
    environment_id uuid NOT NULL,
    session_id uuid NOT NULL,
    process_epoch bigint NOT NULL,
    id uuid NOT NULL,
    slot smallint NOT NULL CHECK (slot BETWEEN 0 AND 15),
    policy jsonb NOT NULL,
    policy_digest bytea NOT NULL CHECK (octet_length(policy_digest) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (environment_id, id),
    UNIQUE (environment_id, session_id, process_epoch, slot),
    UNIQUE (environment_id, session_id, process_epoch, policy_digest),
    FOREIGN KEY (environment_id, session_id, process_epoch)
        REFERENCES session_processes(environment_id, session_id, epoch),
    CONSTRAINT session_mcp_scopes_policy_check CHECK ((
        policy = '{"kind":"unrestricted"}'::jsonb
        OR (jsonb_typeof(policy) = 'object'
            AND policy->>'kind' = 'allow'
            AND jsonb_typeof(policy->'rules') = 'array'
            AND policy - ARRAY['kind', 'rules'] = '{}'::jsonb)
    ) IS TRUE)
);
