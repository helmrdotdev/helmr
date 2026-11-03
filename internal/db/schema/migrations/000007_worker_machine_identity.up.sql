-- A worker host proves itself with a durable machine key it generated at
-- enrollment: the Control Plane keeps only the public key and mints short-lived
-- host credentials against a signed challenge. Bearer host secrets are gone;
-- hosts holding one enroll again with a key.
DROP TABLE worker_host_secrets;

CREATE TABLE worker_host_keys (
    id uuid PRIMARY KEY,
    worker_group_id uuid NOT NULL,
    worker_host_id uuid NOT NULL,
    -- An Ed25519 public key. A key is registered once and never again, so a
    -- revoked key can neither authenticate nor enroll.
    public_key bytea NOT NULL UNIQUE CHECK (octet_length(public_key) = 32),
    claim_version bigint NOT NULL DEFAULT 1 CHECK (claim_version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT worker_host_keys_worker_scope_fkey FOREIGN KEY (worker_host_id, worker_group_id)
        REFERENCES worker_hosts(id, worker_group_id)
        ON DELETE RESTRICT
);

CREATE UNIQUE INDEX worker_host_keys_one_active_idx ON worker_host_keys (worker_host_id) WHERE revoked_at IS NULL;

-- A single-use enrollment token admits exactly one host to its group before it
-- expires. A group's reusable token stays in worker_group_tokens for fleets.
CREATE TABLE worker_enrollment_tokens (
    id uuid PRIMARY KEY,
    worker_group_id uuid NOT NULL REFERENCES worker_groups(id),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    worker_host_id uuid,
    CHECK (expires_at > created_at),
    CHECK ((consumed_at IS NULL) = (worker_host_id IS NULL)),
    FOREIGN KEY (worker_host_id, worker_group_id) REFERENCES worker_hosts(id, worker_group_id)
);
