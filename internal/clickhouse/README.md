# Historical telemetry

PostgreSQL owns accepted events and log chunks in a durable outbox. The
Dispatcher projects that outbox into ClickHouse; the Control Plane reads
history with tenant-scoped, sequence-ordered `FINAL` queries. Redis remains the
live event projection. A failed or ambiguous ClickHouse acknowledgment leaves
the PostgreSQL claim retryable; outbox retry counts fence acknowledgments.

## Retention and representation

`accepted_at` is the original PostgreSQL outbox `created_at`, in UTC millisecond
precision. Both tables partition by its date and become eligible for TTL
cleanup 90 days after acceptance. Retries preserve this value and therefore
cannot move the logical record to a new partition or extend retention.
`observed_at` describes the event occurrence; `ingested_at` records delivery and
versions `ReplacingMergeTree` replacements. These clocks have different roles.

TTL uses whole-part background cleanup. Ninety days is eligibility for deletion,
not an exact API visibility cutoff; parts can remain until every row in the part
expires and ClickHouse performs cleanup. Readers retain `FINAL` because physical
replacement is asynchronous. Keep the tenant/subject/sequence sorting keys.

Run-log `String` stores arbitrary raw bytes, including NUL and invalid UTF-8.
Base64 belongs only to the API response boundary. Structured records use a
materialized `level LowCardinality(String)` for level filters; stdout/stderr
JSON-looking content does not become a structured record. Event body remains an
opaque JSON string. Run-log lease IDs are required, event references remain
nullable, and 64-bit cursors are unchanged.

## Ingestion envelope

The ingester claims at most 10,000 rows or 16 MiB of admitted payload, whichever
bound is reached first. PostgreSQL's stored generated `ingest_size_bytes`
computes the exact normalized event-message/JSON byte count on writes, or uses
the admitted log size. Claim candidate scans need not repeatedly detoast large
JSON payloads just to size rows they will leave for a later batch.

The byte ceiling excludes metadata, PostgreSQL result objects, Native block
buffers and compression scratch space. It is not a process-memory limit or a
ClickHouse recommendation. ClickHouse's [insert guidance](https://clickhouse.com/docs/best-practices/selecting-an-insert-strategy)
recommends row batching, commonly 10,000–100,000 rows; large individual records
must still obey the client byte budget. Local writer/claim measurements support
16 MiB as the initial operating point. Larger envelopes consumed substantially
more memory without consistent writer throughput gains. Deployment hardware,
network latency and concurrent Dispatcher duties still require load validation.

Each ingestion cycle starts no sooner than one second after the preceding
cycle's start; a slower cycle can continue immediately. Rows accumulate unleased
in PostgreSQL. Event/log sends remain serial, with explicit `async_insert=0`,
Native over HTTP, and LZ4. This targets roughly one insert per table per second
per ingester; each accepted-date partition can create its own part, and multiple
ingesters multiply the rate. A single serial writer waiting for durable async
flush cannot coalesce its own next request. Async server-buffer thresholds are
not client request-size ceilings, and version-dependent defaults are not relied on.

Each claim, write and acknowledgment shares a 25-second deadline within the
30-second lease. Errors retain the existing two-second retry backoff. Sparse
historical projection can wait roughly one cycle plus database/network work;
this is not a public latency SLO. Monitor retry age, backlog and part/merge pressure
before raising limits or adding ingestion concurrency.

## Access and read limits

Bootstrap administration, schema creation, ingestion and historical reads use
separate credentials and ECS execution permissions. Bootstrap is a one-off task;
runtime services receive only their own credentials. Existing-user privilege,
role, profile or password drift fails bootstrap before additive changes. Rotation
and privilege revocation are explicit operator actions.

Initial reader limits are 10,000,000 scanned rows, 1 GiB scanned bytes, 512 MiB
query memory, and a 30-second client deadline (server execution setting permits
1–35 seconds to accommodate the Go driver's deadline cushion). Positive server
constraints prevent disabling ceilings. Overflow throws, and the API discards
any partial rows. Limits are checked during execution and may exceed a threshold
by a processing block. They protect the service; they do not promise arbitrary
filtered-history workloads or Cloud concurrency. A rare-match query can scan far
more rows than its page limit.

## Validation and prerelease rollout

`nix run .#ci-clickhouse` starts isolated local servers from the pinned ClickHouse
package and executes uncached race tests for the shipped schema, binary and
filtered pagination, repeat delivery, deadlines, access separation and bootstrap
recovery. PostgreSQL generated sizing, bounded prefixes and acknowledgment fences
are exercised by `nix run .#ci-postgres`.

Optional writer measurements use `BenchmarkWriterEnvelope` and
`HELMR_TEST_CLICKHOUSE_URL` against an isolated server with the current schema.
Set `HELMR_TEST_CLICKHOUSE_INSERT_MODE=async` only for the comparison benchmark.
`TestTelemetryClaimEnvelopeMeasurement` additionally requires an isolated
PostgreSQL URL and `HELMR_TEST_TELEMETRY_ENVELOPE=1`; set
`HELMR_TEST_TELEMETRY_ENTROPY=random` for the large TOAST candidate-scan probe.
These are synthetic diagnostics, not release SLOs or production savings claims.

The initial PostgreSQL and ClickHouse schemas are greenfield definitions. An
existing deployment with the old initial schema must be reset/reprovisioned by
an explicitly authorized operator before this candidate is deployed. Running
`CREATE TABLE IF NOT EXISTS` against old ClickHouse tables does not upgrade them.
Likewise, this change does not provide a PostgreSQL data migration from the old
initial schema. Preserve any required data before an approved reset.

Prepare the combined Product/Cloud revision, inspect the target schema and
credentials, then authorize the environment reset and rollout. Bootstrap access
before schema migration; migration failure must prevent service rollout. Verify
both deployment/run identities, binary logs, filtered page cursors, late/replayed
records and reader denials in the actual Cloud service. Local tests do not prove
its inherited settings, grants, backup policy, workload, or long-duration cleanup.
