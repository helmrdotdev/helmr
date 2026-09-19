# PostgreSQL resource contracts

The initial schema defines the current resource model. SQL in `query/` owns
transactional transitions; `sqlc generate` produces the Go models and queries.
Handwritten lifecycle constants live in `statuses.go`.

## Names and representations

Resource lifecycle columns use `status`, with resource-specific `TEXT` checks.
Run waits have independent `condition_status` and `suspension_status` axes:
condition resolution does not imply execution has resumed. Runtime
`desired_state` and `observed_state`, workspace `desired_state` and `dirty_state`,
and checkpoint VM/filesystem state describe different facts.

Resource IDs use UUIDv7. Foreign keys that copy tenant, workspace, execution, or
worker placement identity validate that complete scope. Definition IDs pin a
particular deployment definition; declared IDs select a definition within a
deployment. Workspace and session creation select the required definition kind
and declared ID in their transactional `INSERT ... SELECT` predicates.

Workspace version rows identify immutable filesystem snapshots. A
`base_workspace_version_id` identifies the snapshot an execution starts from;
head, parent, staged, private, and resume references keep their particular roles.
Secret versions, deployment versions, and runtime versions have their own meaning.
Timestamps use `TIMESTAMPTZ`; duration and size columns state their units.

## Revisions and authority

`revision` is a resource protocol counter, advanced by the transitions that own
that resource. It is neither a count of every row update nor limited to status
changes. Queries define which operations compare and advance it:

| Resource | Revision use |
| --- | --- |
| Secrets | Rotation and revocation compare the current revision and advance it. |
| Schedules | Definition refresh, archival, admitted occurrences, and admission errors advance the revision; claim/retry bookkeeping uses its own fences. |
| Workspaces | Ownership, writer handoffs, publication, recovery, and lifecycle operations coordinate snapshots and authority with the revision. |
| Sessions | Current-run attachment, continuation, committed turn progress, and lifecycle transitions coordinate the session revision; record allocation advances its sequence separately. |
| Runs | Admission, execution, waits, resume, retries, and terminal transitions advance the revision used by execution authority and event snapshots. |
| Workspace processes | Execution and terminal transitions use the process revision; replay preserves the accepted transition. |

A token wait records the running revision supplied by the caller in
`token_registration_run_revision`. Registration increments the run revision and
stores the resulting value in `expected_run_revision`. The latter tracks the
revision after coordinated wait transitions and fences subsequent checkpoint,
resume, and release operations. Other wait kinds likewise retain the revision
established by their registration/transition. SQL comments document these
before/after meanings beside the columns.

Generation, epoch, sequence, attempt, claim-version, and checkpoint/resume
request/acknowledgment counters remain separate authority or ordering contracts.
They must not be substituted for a resource revision. Telemetry
`snapshot_version` identifies the resource revision represented by an event,
independently of delivery attempts.

## Invariant ownership

Checks enforce row shapes, lifecycle facts, numeric bounds, and JSON structure.
Business checks and long composite constraints have semantic names. Scoped
foreign keys enforce ownership; partial unique indexes enforce exclusivity among
live resources. Transactional predicates enforce current authority, expected
revisions, lease fences, and definition selection. Application admission validates
request semantics before calling those operations. Tests exercise malformed data,
replay conflicts, cross-scope references, and stale authority at these boundaries.

Run logs require a run lease and positive attempt number. `AppendRunLogChunk`
selects both from the current, fenced run lease and verifies the run's current
attempt before writing the outbox. The outbox retains this provenance without
foreign keys that would tie telemetry retention to execution-row retention.
Event rows may omit lease and attempt fields. The ingester rejects invalid log
provenance and records a failed delivery rather than panicking.

Completed tokens require a completion fingerprint and a SQL-nonnull result;
JSON `null` is a valid result. Completion replay compares the fingerprint and
returns deterministic replay/conflict flags. Run, session, and token metadata
are JSON objects; application payloads, results, and outputs retain arbitrary
JSON values. Session run duration and run duration both allow 5,000 through
86,400,000 milliseconds.

Content-addressed digests, including CAS objects and runtime substrates, are
canonical lowercase `sha256:` plus 64 hex characters in `TEXT`. Lease terminal
and finalization fingerprints, checkpoint ready/failed fingerprints, and wait
registration fingerprints use that same text encoding from their canonical-JSON
producers. Binary cryptographic fingerprints and digests remain 32-byte `BYTEA`.
External format contracts are validated by their own producers and boundaries.

## Current resources, history, and indexes

Current resource rows hold lifecycle and authority. Attempts, workspace versions,
secret versions, records, checkpoints, and delivery outboxes retain their own
history and replay facts. Logical deletion ends availability while preserving
references required by retained history. Runtime closure, loss, and physical
reclamation are distinct: reclamation requires its own evidence. Do not infer
physical deletion from a terminal status.

Correctness indexes support uniqueness, ownership witnesses, and live-resource
fences. Performance indexes support established query paths. Add or remove a
performance index for an observed query need; schema vocabulary changes alone do
not justify new indexes, partitioning, RLS, or a shared locking mechanism.
