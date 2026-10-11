# Historical telemetry

The clean schema contains four histories: platform `events`, `session_logs`,
`computer_preparation_logs`, and `computer_command_logs`. PostgreSQL owns accepted
records and durable export claims. ClickHouse stores their historical projection;
its acknowledgment permits outbox retirement. A failed or ambiguous acknowledgment
leaves the claim retryable. Redis remains the live event projection.

## Retention and representation

All histories partition by the original PostgreSQL acceptance date and become
eligible for deletion 90 days after acceptance. Retries preserve that date and
cannot extend retention. `ingested_at` versions `ReplacingMergeTree` replacements;
readers use `FINAL` because physical replacement happens asynchronously.

Events retain UTC millisecond `accepted_at` and `observed_at` values, opaque JSON
bodies and nullable event references. Diagnostic histories retain microsecond
`accepted_at` and `expires_at`, the producer's nanosecond observation timestamp,
producer epoch, stream sequence and byte offsets. `data` stores arbitrary bytes,
including NUL and invalid UTF-8. Explicit data, gap and end records preserve stream
loss and completion information. JSON-looking stdout remains raw stdout.

Diagnostic sort keys contain Environment, owner, producer epoch, stream and
sequence. The command reader constrains that prefix and excludes expired records
at query time. TTL uses whole-part background cleanup: eligibility does not mean
immediate physical deletion. Event history retains its existing TTL visibility
semantics; diagnostic API expiry is enforced separately from background deletion.

## Ingestion envelope

The platform event ingester claims at most 10,000 rows or 16 MiB of admitted
message/JSON payload. Claims, writes and acknowledgments share a 25-second deadline
inside a 30-second claim. Cycles start at least one second apart; errors retain the
two-second retry backoff. Diagnostic admission and export use their separately
configured source, Environment, global queue, record and byte bounds. The event
budget is not a diagnostic policy or a process-memory ceiling.

Writers send typed Native batches over HTTP with LZ4 and explicit `async_insert=0`.
Row-local encoding failures rebuild the batch without the rejected row; healthy
neighbors remain deliverable. ClickHouse's [insert guidance](https://clickhouse.com/docs/best-practices/selecting-an-insert-strategy)
recommends row batching, commonly 10,000–100,000 rows, subject to payload budgets.
A serial writer waiting for durable async flush cannot coalesce its own next
request. Deployment load, backlog and part/merge pressure require actual measurement.

## Access and read limits

Bootstrap administration, schema creation, ingestion and reads use separate
credentials. Runtime services receive only their own credentials. Existing-user
privilege, role, profile or password drift fails bootstrap before additive changes.
Rotation and privilege revocation remain explicit operator actions.

Initial reader limits are 10,000,000 scanned rows, 1 GiB scanned bytes, 512 MiB query
memory and a 30-second client deadline. The server execution setting permits 1–35
seconds for the Go driver's deadline cushion. Positive constraints prevent
disabling ceilings. Overflow throws and readers discard partial results; a page
limit alone does not bound scanned data.

## Validation and prerelease rollout

`nix run .#ci-clickhouse` starts isolated servers from the pinned package and runs
uncached race tests for the exact four-table schema, binary records, command stream
pagination, repeated delivery, retention, deadlines, access and bootstrap recovery.
Tests joining PostgreSQL acceptance to ClickHouse require the PostgreSQL test
configuration as well. `nix run .#ci-postgres` covers PostgreSQL ownership and claim
fences. Optional `BenchmarkWriterEnvelope` and `TestWriterInsertStrategyMeasurement`
use an explicitly configured isolated ClickHouse server; the latter requires
`HELMR_TEST_CLICKHOUSE_INSERT_PROBE=1`. They report synthetic measurements, not SLOs.
`TestTelemetryClaimEnvelopeMeasurement` uses an isolated PostgreSQL URL and
`HELMR_TEST_TELEMETRY_ENVELOPE=1`; `HELMR_TEST_TELEMETRY_ENTROPY=random` selects
the large TOAST probe. Those measurements support the event 16 MiB starting budget;
they do not set diagnostic limits. The canary writes and reads a scoped platform event.

The initial schemas are greenfield definitions. `CREATE TABLE IF NOT EXISTS` does
not upgrade an old deployment. Reset/reprovisioning, credential changes and rollout
need their own authorization; this source change performs no live table removal.
Verify the combined Product/Cloud revision, selected histories, actual export and
reader grants in the target service before release. Local tests do not prove Cloud
settings, workload, backup policy or long-duration physical TTL cleanup.
