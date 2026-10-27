DROP TABLE session_mcp_scopes;

ALTER TABLE agent_definitions
    DROP COLUMN runtime_mcp_policy;

ALTER TABLE template_definitions
    DROP COLUMN network_policy,
    DROP COLUMN prepare_network_policy;
