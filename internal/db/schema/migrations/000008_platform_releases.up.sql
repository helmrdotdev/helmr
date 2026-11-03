-- A release the deployment published to the release-only platform store for
-- its worker hosts: the signed index, its signature and the host assets the
-- index lists. Hosts install and update only from these, through Control
-- Plane-issued read grants.
CREATE TABLE platform_releases (
    version text PRIMARY KEY CHECK (version ~ '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'),
    index_digest text NOT NULL CHECK (index_digest ~ '^sha256:[0-9a-f]{64}$'),
    index_size_bytes bigint NOT NULL CHECK (index_size_bytes > 0),
    signature_digest text NOT NULL CHECK (signature_digest ~ '^sha256:[0-9a-f]{64}$'),
    signature_size_bytes bigint NOT NULL CHECK (signature_size_bytes > 0),
    published_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE platform_release_objects (
    version text NOT NULL REFERENCES platform_releases(version),
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    PRIMARY KEY (version, name)
);

CREATE INDEX platform_release_objects_digest_idx ON platform_release_objects (digest);
