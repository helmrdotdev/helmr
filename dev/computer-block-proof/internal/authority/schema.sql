-- Development-only relational model. Not a product migration.
CREATE TABLE environments (
  id text PRIMARY KEY, org_id text NOT NULL, project_id text NOT NULL,
  UNIQUE (org_id, project_id, id)
);
CREATE TABLE cas_object_lifetimes (
  digest text PRIMARY KEY, retired_at timestamptz,
  available boolean GENERATED ALWAYS AS (retired_at IS NULL) STORED,
  UNIQUE (digest, available)
);
CREATE TABLE cas_objects (
  org_id text NOT NULL, digest text NOT NULL, size_bytes bigint NOT NULL,
  media_type text NOT NULL,
  required boolean GENERATED ALWAYS AS (true) STORED,
  PRIMARY KEY (org_id, digest), UNIQUE (org_id, digest, size_bytes, media_type),
  FOREIGN KEY (digest, required) REFERENCES cas_object_lifetimes(digest, available)
);
-- Preserve the existing cascade hazard so the collector must protect artifacts.
CREATE TABLE artifacts (
  id text PRIMARY KEY, org_id text NOT NULL, digest text NOT NULL,
  size_bytes bigint NOT NULL, media_type text NOT NULL,
  FOREIGN KEY (org_id,digest,size_bytes,media_type)
    REFERENCES cas_objects(org_id,digest,size_bytes,media_type) ON DELETE CASCADE
);
CREATE TABLE computers (
  environment_id text NOT NULL REFERENCES environments(id), id text NOT NULL,
  epoch bigint NOT NULL, head_id text,
  PRIMARY KEY (environment_id,id)
);
-- Wrapped bytes here are fixture data; no provider unwrap/delivery is modeled.
CREATE TABLE computer_keys (
  environment_id text NOT NULL, computer_id text NOT NULL, id text NOT NULL,
  wrapped_key bytea, retired_at timestamptz,
  available boolean GENERATED ALWAYS AS (retired_at IS NULL) STORED,
  PRIMARY KEY (environment_id,computer_id,id),
  UNIQUE (environment_id,computer_id,id,available),
  CHECK ((retired_at IS NULL) = (wrapped_key IS NOT NULL)),
  FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id)
);
ALTER TABLE computers ADD COLUMN write_key_id text;
ALTER TABLE computers ADD COLUMN required_key boolean GENERATED ALWAYS AS (true) STORED;
ALTER TABLE computers ADD FOREIGN KEY (environment_id,id,write_key_id,required_key)
  REFERENCES computer_keys(environment_id,computer_id,id,available) ON DELETE RESTRICT;
CREATE TABLE computer_objects (
  environment_id text NOT NULL, computer_id text NOT NULL, digest text NOT NULL,
  org_id text NOT NULL, project_id text NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes > 0), media_type text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('segment','index','root')),
  rank integer NOT NULL CHECK (rank >= 0),
  certified_at timestamptz,
  certified boolean GENERATED ALWAYS AS (certified_at IS NOT NULL) STORED,
  certified_org_id text GENERATED ALWAYS AS
    (CASE WHEN certified_at IS NOT NULL THEN org_id END) STORED,
  required boolean GENERATED ALWAYS AS (true) STORED,
  PRIMARY KEY (environment_id,computer_id,digest),
  UNIQUE (environment_id,computer_id,digest,rank),
  UNIQUE (environment_id,computer_id,digest,rank,certified),
  UNIQUE (environment_id,computer_id,digest,certified),
  FOREIGN KEY (org_id,project_id,environment_id) REFERENCES environments(org_id,project_id,id),
  FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id),
  FOREIGN KEY (digest,required) REFERENCES cas_object_lifetimes(digest,available),
  FOREIGN KEY (certified_org_id,digest,size_bytes,media_type)
    REFERENCES cas_objects(org_id,digest,size_bytes,media_type) MATCH SIMPLE ON DELETE RESTRICT
);
CREATE TABLE computer_object_keys (
  environment_id text NOT NULL, computer_id text NOT NULL, digest text NOT NULL,
  key_id text NOT NULL, required boolean GENERATED ALWAYS AS (true) STORED,
  PRIMARY KEY (environment_id,computer_id,digest,key_id),
  FOREIGN KEY (environment_id,computer_id,digest)
    REFERENCES computer_objects(environment_id,computer_id,digest) ON DELETE CASCADE,
  FOREIGN KEY (environment_id,computer_id,key_id,required)
    REFERENCES computer_keys(environment_id,computer_id,id,available) ON DELETE RESTRICT
);
CREATE TABLE computer_object_edges (
  environment_id text NOT NULL, computer_id text NOT NULL, parent_digest text NOT NULL, child_digest text NOT NULL,
  parent_rank integer NOT NULL, child_rank integer NOT NULL,
  required boolean GENERATED ALWAYS AS (true) STORED,
  PRIMARY KEY (environment_id,computer_id,parent_digest,child_digest),
  CHECK (child_rank < parent_rank),
  FOREIGN KEY (environment_id,computer_id,parent_digest,parent_rank)
    REFERENCES computer_objects(environment_id,computer_id,digest,rank) ON DELETE CASCADE,
  FOREIGN KEY (environment_id,computer_id,child_digest,child_rank,required)
    REFERENCES computer_objects(environment_id,computer_id,digest,rank,certified) ON DELETE RESTRICT
);
CREATE TABLE computer_versions (
  environment_id text NOT NULL, computer_id text NOT NULL, id text NOT NULL,
  parent_id text, PRIMARY KEY (environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id),
  FOREIGN KEY (environment_id,computer_id,parent_id) REFERENCES computer_versions(environment_id,computer_id,id)
);
CREATE TABLE computer_version_roots (
  environment_id text NOT NULL, computer_id text NOT NULL, version_id text NOT NULL, digest text NOT NULL,
  page_offset bigint NOT NULL CHECK (page_offset >= 0), capacity bigint NOT NULL CHECK (capacity > 0),
  required boolean GENERATED ALWAYS AS (true) STORED,
  PRIMARY KEY (environment_id,computer_id,version_id),
  FOREIGN KEY (environment_id,computer_id,version_id) REFERENCES computer_versions(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,digest,required) REFERENCES computer_objects(environment_id,computer_id,digest,certified)
);
ALTER TABLE computers ADD FOREIGN KEY (environment_id,id,head_id)
  REFERENCES computer_version_roots(environment_id,computer_id,version_id);
CREATE TABLE checkpoints (
  environment_id text NOT NULL, id text NOT NULL, computer_id text NOT NULL,
  epoch bigint NOT NULL, capture_id text NOT NULL,
  scratch text NOT NULL, memory text NOT NULL, runtime text NOT NULL, metadata text NOT NULL,
  version_id text, PRIMARY KEY (environment_id,id),
  UNIQUE (environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id),
  FOREIGN KEY (environment_id,computer_id,version_id) REFERENCES computer_version_roots(environment_id,computer_id,version_id)
);
CREATE TABLE computer_publications (
  environment_id text NOT NULL, id text NOT NULL, computer_id text NOT NULL,
  epoch bigint NOT NULL, predecessor_id text, checkpoint_id text, capture_id text,
  write_key_id text NOT NULL,
  retained_write_key text GENERATED ALWAYS AS
    (CASE WHEN status IN ('constructing','registered') THEN write_key_id END) STORED,
  required_key boolean GENERATED ALWAYS AS (true) STORED,
  source_pin text, manifest text, root_digest text, page_offset bigint, capacity bigint,
  status text NOT NULL CHECK (status IN ('constructing','registered','published','abandoned')),
  result_id text, PRIMARY KEY (environment_id,id),
  UNIQUE (environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,write_key_id)
    REFERENCES computer_keys(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,retained_write_key,required_key)
    REFERENCES computer_keys(environment_id,computer_id,id,available) ON DELETE RESTRICT,
  CHECK ((checkpoint_id IS NULL) = (capture_id IS NULL)),
  CHECK (status NOT IN ('registered','published') OR
    (manifest IS NOT NULL AND root_digest IS NOT NULL AND page_offset IS NOT NULL AND capacity IS NOT NULL AND page_offset >= 0 AND capacity > 0)),
  CHECK ((status = 'published') = (result_id IS NOT NULL)),
  CHECK (status NOT IN ('published','abandoned') OR source_pin IS NULL),
  FOREIGN KEY (environment_id,computer_id) REFERENCES computers(environment_id,id),
  FOREIGN KEY (environment_id,computer_id,predecessor_id) REFERENCES computer_versions(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,source_pin) REFERENCES computer_version_roots(environment_id,computer_id,version_id),
  FOREIGN KEY (environment_id,computer_id,checkpoint_id) REFERENCES checkpoints(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,result_id) REFERENCES computer_versions(environment_id,computer_id,id)
);
CREATE TABLE computer_publication_objects (
  environment_id text NOT NULL, computer_id text NOT NULL, publication_id text NOT NULL, digest text NOT NULL,
  PRIMARY KEY (environment_id,computer_id,publication_id,digest),
  FOREIGN KEY (environment_id,computer_id,publication_id) REFERENCES computer_publications(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,digest) REFERENCES computer_objects(environment_id,computer_id,digest)
);
-- Representative owner rows, not a replacement for the production lifecycle.
CREATE TABLE attempts (
  environment_id text NOT NULL, computer_id text NOT NULL, id text NOT NULL, base_version text NOT NULL,
  consumer_excluded boolean NOT NULL DEFAULT false,
  retry_needed boolean NOT NULL DEFAULT true,
  retained_version text GENERATED ALWAYS AS
    (CASE WHEN NOT consumer_excluded OR retry_needed THEN base_version END) STORED,
  PRIMARY KEY (environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,base_version) REFERENCES computer_versions(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,retained_version) REFERENCES computer_version_roots(environment_id,computer_id,version_id)
);
CREATE TABLE waits (
  environment_id text NOT NULL, computer_id text NOT NULL, id text NOT NULL, base_version text NOT NULL,
  resume_version text NOT NULL, transferred boolean NOT NULL DEFAULT false,
  retained_base text GENERATED ALWAYS AS (CASE WHEN NOT transferred THEN base_version END) STORED,
  retained_resume text GENERATED ALWAYS AS (CASE WHEN NOT transferred THEN resume_version END) STORED,
  PRIMARY KEY (environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,base_version) REFERENCES computer_versions(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,resume_version) REFERENCES computer_versions(environment_id,computer_id,id),
  FOREIGN KEY (environment_id,computer_id,retained_base) REFERENCES computer_version_roots(environment_id,computer_id,version_id),
  FOREIGN KEY (environment_id,computer_id,retained_resume) REFERENCES computer_version_roots(environment_id,computer_id,version_id)
);
