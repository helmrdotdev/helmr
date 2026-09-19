# Session lifecycle storage and protocol contract

This document fixes the package 1 contract. The [overview](README.md) describes the
authoring API; [validation](validation.md) identifies implemented proof and remaining
integration work. JSON examples below describe the new protocol, not deployed routes.

## Identities and authority

All public resources are authenticated and environment scoped. UUIDs are identifiers,
not capabilities. A Turn ID is its ordinary input ID, not an independently created
global resource. A message never advances the ordinary input cursor.

The customer-runtime authority tuple is `session_id, turn_id, run_id, attempt_number,
run_generation` plus the existing live Run lease fence. Session output explicitly
omits `turn_id`. Generation alone never substitutes for lease/attempt/physical writer
validation. Stop and all conflicting admissions/settlements use the same locked
Session row. Existing secret/owner/Run/Workspace/lease lock order must remain globally
consistent; no new path may acquire those locks in reverse order.

The package 1 Session owner serializes new lifecycle-operation claims after locking
Session/input authority. All callers of these operation namespaces must keep that
order; no caller may hold one of those claims while waiting for its Session. Worker
paths retain the earlier attempt-secret and physical authority order around this
owner. Existing operations with a different claim-first order are not reused as
alternate callers of the new claim namespace. Cutover removes their superseded paths.

`turn.onMessage` installs one handler before declaring generation-bound readiness.
Readiness must be acknowledged before a later request-output publication can become
visible. Messages deliver serially in their accepted event-sequence order for that
Turn and retain their immutable target; no separate public message cursor is added. An admitted
callback may publish Turn output, but cannot call complete/fail itself. Stop control
delivery is independent of callbacks and consuming waits. A handler that never
returns cannot block forced process convergence indefinitely.

## Storage constraints

These are final logical relations; schema cutover removes the legacy direction-based
record model instead of maintaining two authorities. Source SQL and the authoritative
owner implement each constraint; table layout cannot be treated as an application
request bus.

| Relation | Stored authority and required constraints |
| --- | --- |
| `sessions` | Environment, deployment/Workspace identity, current Run/generation, revision, status, active input ID, FIFO allocated/committed cursors, event allocator, close boundary, hold ID/reason/Run generation. `open/closing/closed/failed` only. At most one active input. Hold identity and reason present together. Active input belongs to this Session and is the next contiguous unsettled input. |
| `session_turns` | Input identity/sequence/data/source, execution binding, status, optional interrupt intent, terminal event reference. Unique `(session_id, sequence)` and `(session_id,id)`; only queued/running/completed/failed/interrupted. A terminal reference exists exactly for terminal status and belongs to this Turn. No duplicate mutable result copy. |
| `session_messages` | Message identity, exact Turn/execution, input data, disposition and optional outcome. accepted → handling → handled/rejected/unknown, or accepted → rejected on fencing. No redelivery after unknown without native reconciliation. Same key/payload has one target and one disposition history. |
| `session_events` | One unique `(session_id, sequence)` and stable event ID; fixed kind, optional subject Turn/message, source/provenance, JSON body. Lifecycle events are runtime-owned. Terminal event stores result-or-error and committed Workspace version. |
| Existing `idempotency_claims` | Scoped operation key, canonical fingerprint, bound route/target and durable receipt/rejection. Reuse current transaction owner; do not add a second operation registry. |
| Existing `run_waits` / owned child relation | Add optional Turn/execution binding only for operations started inside a Turn. Validate at registration, checkpoint/resume attachment, completion and consumed acknowledgment. Global Token identity/result stays unchanged. |

Cross-resource foreign keys include environment/Session where applicable. A terminal
event and head cannot name another Workspace or input. Result absence omits the
terminal event's `result` member (JSON extraction yields SQL NULL); an explicit
`result: null` remains present JSON data. Queue/event counters stay within
JavaScript-safe integer bounds. Exhaustion rejects rather than wraps. Current physical
names are `status`, `revision`, `base_workspace_version_id`; no older-name aliases.

The initial schema is rewritten for disposable prerelease initialization. No upgrade
migration/backfill is provided. Intermediate package code is not a deployable mixed
schema/protocol release. Shared database reset needs its own authorized operation.

## Linearization and outcomes

| Operation | Decision under Session authority | Observable outcome |
| --- | --- | --- |
| Receive | Check ready dispatch and no active input; activate the next FIFO input under current execution. | Turn handle after deployed input validation; known validation issues settle failed before customer work. Validation crash follows recovery. |
| Automatic send | Require open+ready; bind active target or FIFO admission exactly once. | enqueued Turn or accepted message. Active unready/parked target rejects; never fallback. |
| Enqueue | Require open; allocate input at FIFO tail even if held. | Queued Turn; no promise of execution or schema success. |
| Exact send | Check exact active Turn, writable phase, live ready execution. Closing may allow valid existing interaction. | Accepted message; later handled/rejected/unknown event. |
| Complete/fail | Fence new callback admission; reconcile admitted callbacks/output; validate result and physical Workspace proof; commit terminal event+cursor+head together. | completed or failed; no terminal success after accepted stop. |
| Interrupt | Require exact active Turn; atomically record intent and new hold, revoke new Turn work admission. | Acceptance plus hold. Later interrupted only after convergence/proof; otherwise recovery_required. |
| Output | Require exact current producer and writable scope; append through the same locked authority. | Event receipt. Already retained output never implies work completion. |
| Resume | Compare current hold; require settled input/no unresolved execution and safe Workspace boundary. | New ready dispatch, same FIFO. Stale hold/not settled rejects. |
| Close | Capture accepted FIFO end and status closing; reject ordinary admission. | Drain; null receive before release; holds survive. Empty safe close need not require resume. |
| Recover | Compare hold + exact active input (nullable), prove exclusion/head and reconcile external work. | Failed/interrupted terminal or Session-only recovery; new recovered hold, never success. |

Complete/fail begins a settlement barrier, not a terminal event. It may fence new
messages while allowing already-admitted tracked operations to drain. A concurrent
stop can still win until the final terminal transaction commits. Main flow must not
admit new work after requesting completion. At the worker boundary, message delivery
IDs distinguish already admitted callbacks from new work; the barrier must not be
implemented by rejecting their required cleanup writes indiscriminately.

Unknown callback or output delivery prevents false success. Reconcile a lost append
by immutable operation identity, not by appending new output. For native operations,
the app must reconcile before acknowledging the handler; after unknown disposition
enters recovery, privileged repair cannot turn that uncertain Turn into success.
Queued messages rejected by settlement do not consume later ordinary input.

## Idempotency and receipts

An optional user key is scoped to environment, Session and operation. Exact-target
operations include target in their fingerprint. Without a user key, the SDK assigns
an operation identity retained across transport retries of that invocation; calling
the API again intentionally is a new operation. Fingerprint mismatch is conflict.

Bind route and rejections atomically with the authoritative admission decision.
Business rejections commit their receipt rather than rolling back the operation
claim. Malformed envelopes/authentication failures occur before admission and do not
reserve a target. A rejected retry cannot become a later enqueue/message/resume.
Idempotency is guaranteed within retained operation history only; an expired key
must not be offered as permission to replay an uncertain external effect.

```ts
type Admission =
  | { id: string; kind: "enqueued"; turnId: string }
  | { id: string; kind: "messaged"; turnId: string; messageId: string };
type InterruptReceipt = {
  id: string; turnId: string; holdId: string; status: "accepted";
};
type OutputReceipt = {
  id: string; sequence: number; sessionId: string; turnId: string | null;
  runId: string; attemptNumber: number; runGeneration: number;
};
```

The SDK maps identity receipts to the selected reference API: `enqueue` returns
`TurnRef`; `send` returns `{kind: "enqueued", turn: TurnRef}` or
`{kind: "messaged", turn: TurnRef, message: {id: messageId, status: "accepted"}}`.
Exact `turn.send` returns that message receipt. The reference exposes send, interrupt
and retrieve; it is an address, not an authority grant. Wire DTOs store identity
only; reference construction is required SDK behavior, not an optional alternative.
All successful operation receipts acknowledge durable admission/settlement specified
by that operation. They do not assert provider consumption or remote completion.

Output retry is stricter than reading a historical event. Before returning a write
success, revalidate current writable authority and identical producer scope. A new
Run/attempt cannot adopt an earlier producer's receipt as fresh admission. After
stop/terminal, writes reject even if the old event exists; the event remains readable.
Do not grant a native effect from a cached receipt or an event-history lookup.

## Event envelope and retention

REST uses snake_case field names, SDK uses camelCase. All events have `id`,
`session_id`, `sequence`, `created_at`, `kind`, `data`, nullable `turn_id` and nullable
`provenance` (`run_id`, `attempt_number`, `run_generation`, `deployment_id`). External
admission need not have execution provenance yet. Sequence is monotonic, never time
sorted; one transaction can append several adjacent events.

Required event kinds are `output`, `turn.enqueued`, `turn.started`,
`turn.interrupt_requested`, `turn.completed`, `turn.failed`, `turn.interrupted`,
`message.accepted`, `message.handled`, `message.rejected`, `message.unknown`,
`session.closing`, `session.closed`, `session.failed`, `session.held`,
`session.resumed`, `session.recovered`. `message.handling` is durable delivery state,
not a mandatory extra public event. Session creation is observed through start/read.
Final completed data contains optional `result` and required `workspace_version_id`;
failed contains `error`, interrupted carries stop identity, both retain the proven
Workspace reference. Application output cannot choose the envelope kind.

Every message event's data requires `message_id`, equal to the admission receipt's
message ID, and the envelope requires its exact `turn_id`. `message.accepted` also
contains `message` (the submitted application JSON). `message.handled` acknowledges
handler completion; it does not assert provider consumption. `message.rejected`
contains `code` (`schema_invalid`, `handler_rejected`, `turn_settling` or
`turn_stopping`) and optional application JSON `details`. `message.unknown` contains
`code` (`handler_failed` or `execution_lost`) and optional diagnostic JSON `details`;
it cannot be relabeled as rejection after an uncertain effect. Operation IDs,
message IDs and native provider request IDs remain distinct.

`GET events?after=0&limit=100` reads records strictly after the sequence, default
limit 100, maximum 1000. Return `records`, `next_after` (last returned sequence, or
unchanged after for empty page), `has_more`, `retained_after`. A cursor older than
the retained prefix returns `cursor_expired` with `retained_after`; a cursor beyond
the allocated end is `invalid_cursor`. No live subscription endpoint in this slice.

Initially retain Session events for the retained Session lifetime; no independent
trimming job is introduced. Operation claims keep the existing 30-day idempotency
window. After expiry the same key can represent a new invocation, so the application
must reconcile an uncertain native action rather than treating a later successful
write as proof that it never ran. Terminal Turn retrieval derives result from its
retained terminal event. Future event retention may remove only an agreed
prefix with an explicit gap response; it cannot silently erase a retained result's
authoritative terminal reference. Session not found and expired cursor are distinct.

## HTTP, worker transport and errors

Session retrieval includes `status`, nullable `current_run_id`, nullable
`active_turn_id` and `dispatch`: `{state: "ready"}` or `{state: "held", hold_id,
reason}`. Hold reasons are `interrupt_requested`, `interrupted`, `recovery_required`
or `recovered`; interrupt_requested is not resumable. Lifecycle transitions replace
hold identity when establishing a new hold, so stale resume cannot clear it.
Turn retrieval includes `id`, `sequence`, `status`, `interrupt_requested` and
`accepts_messages`; terminal views include the authoritative event and Workspace
reference, with `result` only when present and `error` only on failure. Readiness is
observational, not a promise that a later message will still be accepted.

Under authenticated `/v1/sessions/:id` (and equivalent console scope), operations are
`POST send`, `enqueue`, `close`, `resume`, `recover`; `GET events`; `GET turns/:id`;
`POST turns/:id/messages`, `turns/:id/interrupt`. Submission body is `{data,
idempotency_key?}`; ordinary payload cannot set a runtime target/mode. Close/interrupt
accept key only; resume requires `hold_id`. Recover requires `hold_id`, explicit
nullable `turn_id`, `workspace_version_id`, `reconciliation_ref`, optional key; a
non-null Turn requires failed/interrupted `disposition`, null forbids it.

Errors use existing API error transport with stable codes: `session_not_found`,
`turn_not_found` (404); `invalid_request`, `invalid_cursor` (400); `forbidden` (403);
`session_not_open`, `session_held`, `turn_not_active`, `turn_not_ready`,
`turn_stopping`, `turn_unsettled`, `stale_execution`, `stale_hold`, `not_settled`,
`idempotency_conflict`, `recovery_required` (409); `cursor_expired` (410).
Runtime schema/handler rejection becomes a disposition event, not a synchronous
promise that API schema validation happened. HTTP transport ambiguity stays unknown
until retry/read reconciliation; it is not a new business rejection code.

Worker commands carry existing authenticated worker/lease fence plus operation UUID,
Session/Turn/execution authority. Specify messages for receive/activation, readiness,
message delivery+ack/reject/unknown, output write, settlement barrier+commit, stop
delivery+convergence and wait-association revocation. Correlation UUID identifies one
request/response; durable operation UUID survives transport retry. Never trust a
customer-supplied terminal event or quiescence boolean. Existing guest/lease/Workspace
proof validates actual finalization; restored execution rechecks its checkpoint and
writer authority. An empty protobuf result field differs from absent only through
explicit optional presence, not by treating both as JSON null.

Permissions: `sessions.read`, `sessions.send` (automatic/enqueue/exact message),
`sessions.interrupt`, `sessions.resume`, `sessions.close`, `sessions.recover`.
Recovery is owner/admin only and requires explicit key grant where an API key is
used. Other lifecycle mutations are available to developer role with appropriate
scope/grant. Worker authority does not come from these public grants.

## Stop and recovery outside ordinary completion

Initial stop terminates the entire owning Run process tree; keeping a provider server
alive is not promised. Stage intent/hold before cancellation, preserving a valid
finalizer. Cooperative quiescence captures current recoverable Workspace state;
forced stop retains the last proven head only after physical writer exclusion.
The database lock itself proves neither process death nor remote rollback.

Public exact Run cancellation remains available under `runs.manage` only if that
Run still owns the Session and no Turn is active under the same lock. Return
Run/Session/hold acceptance, not a terminal claim. An active Turn requires exact-Turn
interrupt; a stale Run never targets its successor. Task cancellation is unchanged.
All internal Actor expiry/loss/forced-failure paths use Session authority too.

Initialization, between-Turn work and cleanup may fail with null active Turn. Hold
against the failed Run/generation, preserve queued inputs and committed outcomes,
and repair with explicit null target. Such repair changes no input cursor and emits
Session recovery only. Non-null recovery settles interrupted if stop won, otherwise
failed. Both require exact current hold, writer exclusion, proven Workspace version
and application/operator reconciliation of external effects; then issue a new hold
and await explicit resume. No claim of native success is manufactured.

Proven pre-entrypoint launch retry and valid checkpoint continuation remain supported.
Uncertain cold execution never automatically replays. Explicit repair/resume starts
the new Run entrypoint, including initialization; the application must reconcile it
to be safe again. No arbitrary program-counter or provider socket restoration.

## Permission admission

For a live, qualified integration, the application binds a pending native callback
to this Turn, provider epoch, request ID and exact action. After authenticating and
validating the reply, it awaits ordinary scoped output containing its opaque
`permission_admitted` record before returning native allow. Helmr interprets neither
that payload nor a universal permission scope.

The **write transaction** is the admission ordering point. Stop first must reject it
even if the local abort signal has not arrived. Write first admits the pending
allow/native start into stop convergence, even if the network start is delayed.
An ambiguous write never grants; history cannot reconstruct a callback or grant.
The local pending request must still be live, and exactly one native response may
be sent. Native denial/questions have their own application mapping; a generic
message saying yes is not a native permission response.

The owning application/provider operation must remain tracked until native resolution
or cancellation. Quiescence cannot be declared while an admitted delayed start may
still run. Killing the local process prevents future local dispatch but does not undo
an already accepted remote effect; unresolved remote effects keep recovery held.
This is cooperative admission ordering, not credential enforcement over arbitrary
customer code. No new public permission/Token primitive is added by this contract.
