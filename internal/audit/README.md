# Audit capture

`audit.Append(ctx, tx, policy, event)` writes one fact in the operation owner's
transaction. The owner must propagate failure so its state change rolls back.
It does not commit, enqueue background work, or deduplicate against audit history.
The leaf `auditpolicy` package carries required finite deployment retention without
pulling database or API dependencies into configuration consumers. Bounded,
authorized search lives in `audit/query`; HTTP response types belong to `api`.

## Add an operation

1. Add an explicit action, target kind and allowed outcomes to `catalog.go`.
2. Use an existing typed Details value or add an allowlisted type and validation
   to `details.go`. Never serialize a request, error, prompt, command or arbitrary
   map. Optional UUIDs use `omitzero`.
3. At the existing authority/transaction owner, retain the actual authenticated
   actor and evaluated decisions. Preserve the transport credential version when
   rebuilding a principal. Record every required permission for compound actions.
   Session actors and worker/service observations are distinct from their initiators.
4. Append only when the durable transition changes. Replays use the owner's
   existing receipt even after its audit row expires. For sensitive reads, select
   and buffer bounded data, append access admission, commit, then release bytes.
5. Classify the HTTP entry point in `controlplane/audit_routes.go`, including an
   explicit exclusion where appropriate. Known refusal boundaries call
   `writeOperationError` or `refuseOperation`; HTTP status alone is not an event.
6. Prove actual PostgreSQL capture, rollback/withheld bytes, replay, authorization,
   safe details and applicable actor/correlation facts. Extend route coverage and
   privacy tests. Run the affected owner tests and dependency/SQL-ownership checks.

A receipt records acceptance or settlement, not an unobserved physical outcome.
`computer.checkpoint_published` records durable verified checkpoint publication;
`computer.restore_completed` records activation plus acknowledged retained-process
controls. `computer.control_settled` records the explicit request receipt.
`mcp.admitted` is authenticated transport admission; the dispatched owner checks
fresh business authority or reconciles its existing receipt.

After a known failed transaction, bounded independent failure capture can record
the observed refusal. `db.CommitOutcomeUnknown` means a commit acknowledgement was
lost or otherwise inconclusive: do not label that operation denied/failed or retry
its mutation as if rollback were known. Its original transaction may have committed
both the operation and audit fact. Report the uncertainty through operational errors.

## Storage and trust

One append-only table stores bounded inline account identity and authorization
witnesses. Event identifiers have no live-resource foreign keys. Account identity
is a capture-time label, not a legal identity claim. Login credential IDs differ
from provider provenance; verified issuance records carry provider/method details.
Device issuance references the consumed device-code row ID so retained approval
facts can establish its authorizing credential without storing a raw device code.

The application bounds encoded identity to 4 KiB, witnesses/details to 16 KiB each,
causality to 4 KiB and the envelope to 64 KiB. PostgreSQL additionally bounds the
stored JSONB representation with `pg_column_size`. It is not a text rendering of
JSON. Oversized mandatory facts fail closed instead of being truncated.

Runtime has SELECT/INSERT on audit facts, with no UPDATE, DELETE, TRUNCATE, DDL or
reachable stronger role. Migration owns the schema. Maintenance separately has
SELECT/INSERT/DELETE, making it a trusted expiry operator: its SQL privileges can
technically remove unexpired rows. Database owners and compromised trusted producers
remain outside this protection. No cryptographic integrity or independent custody
is claimed.

See the [audit API and operations documentation](../../packages/web/src/content/docs/reference/api/audit-events.md)
for search, retention, maintenance and recovery prerequisites.
