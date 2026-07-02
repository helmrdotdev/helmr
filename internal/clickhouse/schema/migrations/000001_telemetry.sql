CREATE DATABASE IF NOT EXISTS helmr_telemetry;

CREATE TABLE IF NOT EXISTS helmr_telemetry.run_logs (
    cell_id String,
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    session_id Nullable(UUID),
    run_id UUID,
    attempt_id Nullable(UUID),
    run_lease_id Nullable(UUID),
    worker_group_id Nullable(UUID),
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
PARTITION BY (toYYYYMM(observed_at), cell_id)
ORDER BY (org_id, project_id, environment_id, run_id, stream_name, seq);

CREATE TABLE IF NOT EXISTS helmr_telemetry.events (
    cell_id String,
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    subject_kind LowCardinality(String),
    subject_id UUID,
    event_kind String,
    seq UInt64,
    run_id Nullable(UUID),
    deployment_id Nullable(UUID),
    attempt_id Nullable(UUID),
    run_lease_id Nullable(UUID),
    trace_id String,
    span_id String,
    parent_span_id String,
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
PARTITION BY (toYYYYMM(observed_at), cell_id)
ORDER BY (org_id, project_id, environment_id, subject_kind, subject_id, seq);

CREATE TABLE IF NOT EXISTS helmr_telemetry.trace_spans (
    cell_id String,
    org_id UUID,
    project_id UUID,
    environment_id UUID,
    trace_id String,
    span_id String,
    parent_span_id String,
    run_id Nullable(UUID),
    attempt_id Nullable(UUID),
    name String,
    attributes String,
    retention_class LowCardinality(String),
    redaction_class LowCardinality(String),
    observed_at DateTime64(3, 'UTC'),
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY (toYYYYMM(observed_at), cell_id)
ORDER BY (org_id, project_id, environment_id, trace_id, span_id, observed_at);

CREATE TABLE IF NOT EXISTS helmr_telemetry.terminal_output (
    cell_id String,
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
PARTITION BY (toYYYYMM(observed_at), cell_id)
ORDER BY (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream_name, offset_start);

CREATE TABLE IF NOT EXISTS helmr_telemetry.ingest_errors (
    cell_id String,
    org_id UUID,
    stream_kind LowCardinality(String),
    source_kind String,
    source_id String,
    idempotency_key String,
    error String,
    retry_count UInt32,
    observed_at DateTime64(3, 'UTC'),
    ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = MergeTree
PARTITION BY (toYYYYMM(observed_at), cell_id)
ORDER BY (org_id, stream_kind, observed_at, idempotency_key);
