CREATE DATABASE IF NOT EXISTS helmr_telemetry;

CREATE TABLE IF NOT EXISTS helmr_telemetry.events (
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    subject_kind LowCardinality(String),
    subject_id UUID,
    event_kind LowCardinality(String),
    seq UInt64,
    deployment_id Nullable(UUID),
    trace_id String,
    span_id String,
    parent_span_id String,
    traceparent String,
    category LowCardinality(String),
    severity LowCardinality(String),
    source LowCardinality(String),
    message String,
    body String,
    idempotency_key String,
    retention_class LowCardinality(String),
    redaction_class LowCardinality(String),
    observed_at DateTime64(3, 'UTC'),
    accepted_at DateTime64(3, 'UTC'),
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toDate(accepted_at)
ORDER BY (org_id, subject_kind, subject_id, seq)
TTL accepted_at + INTERVAL 90 DAY DELETE
SETTINGS ttl_only_drop_parts = 1;

-- Subject queries constrain the full Environment/owner/producer/stream prefix.
-- Daily partitions serve the 90-day acceptance lifecycle, not tenant routing.
CREATE TABLE IF NOT EXISTS helmr_telemetry.session_logs (
    environment_id UUID,
    session_id UUID,
    producer_epoch Int64,
    stream Enum8('stdout'=1,'stderr'=2),
    sequence Int64,
    through_sequence Int64,
    kind Enum8('data'=1,'gap'=2,'end'=3),
    observed_at_unix_nano Int64,
    data String,
    dropped_bytes Int64,
    complete Bool,
    byte_offset Int64,
    through_byte_offset Int64,
    accepted_at DateTime64(6, 'UTC'),
    expires_at DateTime64(6, 'UTC'),
    ingested_at DateTime64(6, 'UTC') DEFAULT now64(6),
    CONSTRAINT diagnostic_identity CHECK producer_epoch>0 AND sequence>0 AND through_sequence>=sequence,
    CONSTRAINT diagnostic_offsets CHECK byte_offset>=0 AND through_byte_offset>=byte_offset AND through_byte_offset-byte_offset=length(data)+dropped_bytes,
    CONSTRAINT diagnostic_retention CHECK expires_at=accepted_at+INTERVAL 90 DAY,
    CONSTRAINT diagnostic_record CHECK (kind='data' AND sequence=through_sequence AND length(data)>0 AND dropped_bytes=0 AND NOT complete)
        OR (kind='gap' AND length(data)=0 AND dropped_bytes>=through_sequence-sequence+1 AND NOT complete)
        OR (kind='end' AND sequence=through_sequence AND length(data)=0 AND dropped_bytes=0)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toDate(accepted_at)
ORDER BY (environment_id, session_id, producer_epoch, stream, sequence)
TTL expires_at DELETE
SETTINGS ttl_only_drop_parts = 1;


-- Subject queries constrain the full Environment/owner/producer/stream prefix.
-- Daily partitions serve the 90-day acceptance lifecycle, not tenant routing.
CREATE TABLE IF NOT EXISTS helmr_telemetry.computer_preparation_logs (
    environment_id UUID,
    preparation_id UUID,
    producer_epoch Int64,
    stream Enum8('stdout'=1,'stderr'=2),
    sequence Int64,
    through_sequence Int64,
    kind Enum8('data'=1,'gap'=2,'end'=3),
    observed_at_unix_nano Int64,
    data String,
    dropped_bytes Int64,
    complete Bool,
    byte_offset Int64,
    through_byte_offset Int64,
    accepted_at DateTime64(6, 'UTC'),
    expires_at DateTime64(6, 'UTC'),
    ingested_at DateTime64(6, 'UTC') DEFAULT now64(6),
    CONSTRAINT diagnostic_identity CHECK producer_epoch>0 AND sequence>0 AND through_sequence>=sequence,
    CONSTRAINT diagnostic_offsets CHECK byte_offset>=0 AND through_byte_offset>=byte_offset AND through_byte_offset-byte_offset=length(data)+dropped_bytes,
    CONSTRAINT diagnostic_retention CHECK expires_at=accepted_at+INTERVAL 90 DAY,
    CONSTRAINT diagnostic_record CHECK (kind='data' AND sequence=through_sequence AND length(data)>0 AND dropped_bytes=0 AND NOT complete)
        OR (kind='gap' AND length(data)=0 AND dropped_bytes>=through_sequence-sequence+1 AND NOT complete)
        OR (kind='end' AND sequence=through_sequence AND length(data)=0 AND dropped_bytes=0)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toDate(accepted_at)
ORDER BY (environment_id, preparation_id, producer_epoch, stream, sequence)
TTL expires_at DELETE
SETTINGS ttl_only_drop_parts = 1;


-- Subject queries constrain the full Environment/owner/producer/stream prefix.
-- Daily partitions serve the 90-day acceptance lifecycle, not tenant routing.
CREATE TABLE IF NOT EXISTS helmr_telemetry.computer_command_logs (
    environment_id UUID,
    command_id UUID,
    producer_epoch Int64,
    stream Enum8('stdout'=1,'stderr'=2),
    sequence Int64,
    through_sequence Int64,
    kind Enum8('data'=1,'gap'=2,'end'=3),
    observed_at_unix_nano Int64,
    data String,
    dropped_bytes Int64,
    complete Bool,
    byte_offset Int64,
    through_byte_offset Int64,
    accepted_at DateTime64(6, 'UTC'),
    expires_at DateTime64(6, 'UTC'),
    ingested_at DateTime64(6, 'UTC') DEFAULT now64(6),
    CONSTRAINT diagnostic_identity CHECK producer_epoch>0 AND sequence>0 AND through_sequence>=sequence,
    CONSTRAINT diagnostic_offsets CHECK byte_offset>=0 AND through_byte_offset>=byte_offset AND through_byte_offset-byte_offset=length(data)+dropped_bytes,
    CONSTRAINT diagnostic_retention CHECK expires_at=accepted_at+INTERVAL 90 DAY,
    CONSTRAINT diagnostic_record CHECK (kind='data' AND sequence=through_sequence AND length(data)>0 AND dropped_bytes=0 AND NOT complete)
        OR (kind='gap' AND length(data)=0 AND dropped_bytes>=through_sequence-sequence+1 AND NOT complete)
        OR (kind='end' AND sequence=through_sequence AND length(data)=0 AND dropped_bytes=0)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toDate(accepted_at)
ORDER BY (environment_id, command_id, producer_epoch, stream, sequence)
TTL expires_at DELETE
SETTINGS ttl_only_drop_parts = 1;
