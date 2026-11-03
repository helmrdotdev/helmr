DROP TABLE worker_enrollment_tokens;
DROP TABLE worker_host_keys;

CREATE TABLE worker_host_secrets (
    id uuid PRIMARY KEY,
    worker_group_id uuid NOT NULL,
    worker_host_id uuid NOT NULL,
    key_prefix text NOT NULL UNIQUE CHECK (btrim(key_prefix) <> ''),
    claim_version bigint NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    expires_at timestamptz,
    secret_hash bytea NOT NULL UNIQUE CHECK (octet_length(secret_hash) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT worker_host_secrets_worker_scope_fkey FOREIGN KEY (worker_host_id, worker_group_id)
        REFERENCES worker_hosts(id, worker_group_id)
        ON DELETE RESTRICT
);

CREATE UNIQUE INDEX worker_host_secrets_one_active_idx ON worker_host_secrets (worker_host_id) WHERE revoked_at IS NULL;
