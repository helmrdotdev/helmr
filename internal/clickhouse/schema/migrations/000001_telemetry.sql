CREATE DATABASE IF NOT EXISTS helmr_telemetry;

CREATE TABLE IF NOT EXISTS helmr_telemetry.run_logs (
    worker_group_id String,
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    session_id Nullable(UUID),
    run_id UUID,
    run_lease_id Nullable(UUID),
    attempt_number Int32,
    worker_instance_id Nullable(UUID),
    stream_name LowCardinality(String),
    seq UInt64,
    observed_seq UInt64,
    content String,
    size_bytes UInt64,
    idempotency_key String,
    retention_class LowCardinality(String),
    redaction_class LowCardinality(String),
    source LowCardinality(String),
    observed_at DateTime64(3, 'UTC'),
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY (toYYYYMM(observed_at), worker_group_id)
ORDER BY (org_id, project_id, environment_id, run_id, stream_name, seq);

CREATE TABLE IF NOT EXISTS helmr_telemetry.events (
    worker_group_id String,
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    subject_kind LowCardinality(String),
    subject_id UUID,
    event_kind String,
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
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY (toYYYYMM(observed_at), worker_group_id)
ORDER BY (org_id, project_id, environment_id, subject_kind, subject_id, seq);

CREATE TABLE IF NOT EXISTS helmr_telemetry.terminal_outputs (
    worker_group_id String,
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    workspace_id UUID,
    resource_kind LowCardinality(String),
    resource_id UUID,
    stream_name LowCardinality(String),
    offset_start UInt64,
    offset_end UInt64,
    content String,
    size_bytes UInt64,
    idempotency_key String,
    retention_class LowCardinality(String),
    redaction_class LowCardinality(String),
    observed_at DateTime64(3, 'UTC'),
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY (toYYYYMM(observed_at), worker_group_id)
ORDER BY (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream_name, offset_start);
