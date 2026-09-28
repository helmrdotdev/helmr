CREATE DATABASE IF NOT EXISTS helmr_telemetry;

CREATE TABLE IF NOT EXISTS helmr_telemetry.run_logs (
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    run_id UUID,
    run_lease_id UUID,
    attempt_number Int32,
    stream_name LowCardinality(String),
    seq UInt64,
    observed_seq UInt64,
    content String,
    level LowCardinality(String) MATERIALIZED if(stream_name = 'structured', JSONExtractString(content, 'level'), ''),
    size_bytes UInt32,
    idempotency_key String,
    retention_class LowCardinality(String),
    redaction_class LowCardinality(String),
    source LowCardinality(String),
    observed_at DateTime64(3, 'UTC'),
    accepted_at DateTime64(3, 'UTC'),
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toDate(accepted_at)
ORDER BY (org_id, run_id, seq)
TTL accepted_at + INTERVAL 90 DAY DELETE
SETTINGS ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS helmr_telemetry.events (
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    subject_kind LowCardinality(String),
    subject_id UUID,
    event_kind LowCardinality(String),
    seq UInt64,
    run_id Nullable(UUID),
    deployment_id Nullable(UUID),
    run_lease_id Nullable(UUID),
    attempt_number Nullable(Int32),
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

CREATE TABLE IF NOT EXISTS helmr_telemetry.command_logs (
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    command_id UUID,
    stream_name LowCardinality(String),
    observed_seq UInt64,
    seq UInt64,
    content String,
    size_bytes UInt32,
    idempotency_key String,
    retention_class LowCardinality(String),
    redaction_class LowCardinality(String),
    source LowCardinality(String),
    observed_at DateTime64(3, 'UTC'),
    accepted_at DateTime64(3, 'UTC'),
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3),
    CONSTRAINT command_log_stream CHECK stream_name IN ('stdout', 'stderr'),
    CONSTRAINT command_log_size CHECK size_bytes = length(content)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toDate(accepted_at)
ORDER BY (org_id, environment_id, command_id, stream_name, observed_seq)
TTL accepted_at + INTERVAL 90 DAY DELETE
SETTINGS ttl_only_drop_parts = 1;
