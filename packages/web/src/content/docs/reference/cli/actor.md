---
title: helmr actor
description: Start Actors and operate Sessions and Turns.
sidebarLabel: actor
---

# `helmr actor`

```text
helmr actor start ACTOR --workspace WORKSPACE [flags]
helmr actor get SESSION_ID [--json]
helmr actor send SESSION_ID (--data-json JSON | --data-file FILE) [--json]
helmr actor enqueue SESSION_ID (--data-json JSON | --data-file FILE) [--json]
helmr actor turn get SESSION_ID TURN_ID [--json]
helmr actor turn send SESSION_ID TURN_ID (--data-json JSON | --data-file FILE) [--json]
helmr actor turn interrupt SESSION_ID TURN_ID [--json]
helmr actor resume SESSION_ID [--hold HOLD_ID] [--json]
helmr actor events SESSION_ID [--after N] [--limit N] [--json | --jsonl]
helmr actor close SESSION_ID [--json]
```

All commands accept project and environment scope flags. Mutations accept
`--idempotency-key KEY`; reuse the key and exact arguments when retrying an
uncertain response. `--json` prints the server receipt or resource as one JSON
object. A receipt acknowledges the operation, not completion of the work.

`start` accepts `--key`, `--idempotency-key`, and managed-Run options: queue,
concurrency key, priority, TTL, tags, metadata, and retry policy. It requires an
existing Workspace. Send the first input separately after starting the Session.

## Send work and messages

Commands after start address the server-created Session ID. Application data is
any JSON value; Helmr does not assign meaning to an application's `type` field.

- `send` sends a message to the active Turn or enqueues a new Turn when idle.
  Handler readiness does not gate acceptance: messages wait for delivery to that
  same Turn. A settling Turn or held Session rejects the request; it does not
  silently enqueue replacement work.
- `enqueue` always queues a new Turn, including while another Turn is running
  or an open Session is held.
- `turn send` targets exactly the supplied Turn. Use it for answers, approvals
  and other messages that must never reach a different Turn.

Admission receipts include the Turn ID. `turn get` reports message readiness,
interruption requests, and the final result or error. A failed Turn does not by
itself mean that the Session failed.

## Interrupt and resume

`turn interrupt` requests a stop and returns a hold ID. A `stopping` receipt does
not mean execution has stopped yet. Inspect the Turn and Session to observe
convergence. Queued Turns remain retained and do not start automatically.

`resume` reads the current hold and submits that exact ID. If the hold changes
between the read and the mutation, the server rejects the stale request; the CLI
does not fetch a newer hold and retry. Use `--hold` to target a previously observed
hold, particularly when retrying an uncertain resume with an idempotency key.
Resume permits queued work to proceed; it does not restart the interrupted Turn.

## Read the timeline

`events` reads a finite page of the retained Session timeline: inputs, messages,
output and lifecycle events share one durable sequence. Use `next_after` for the
next page. `--after` is a nonnegative safe integer; `--limit` defaults to 100 and
has a maximum of 1000. `--json` includes pagination and retention metadata;
`--jsonl` emits only records. End of a page or provider output is not Turn
completion. Observe a terminal Turn event or use `turn get` for the outcome.

`close` rejects new ordinary admission and drains already accepted FIFO work. Its
receipt may report `closing` until that work settles. Existing holds remain in
place and must be resolved before held work can drain; close does not interrupt
the active Turn or clear a hold. Use `get` to observe the final Session state.

## Reconcile uncertain execution

Recovery is an owner/admin operation with the `sessions.recover` grant, separate
from ordinary resume. Reconcile external effects and identify the Workspace
version before submitting it:

```text
helmr actor recover SESSION_ID HOLD_ID \
  --turn TURN_ID --disposition failed \
  --workspace-version VERSION_ID --reconciliation-ref RECORD \
  [--idempotency-key KEY] [--json]
```

Use `--outside-turn` instead of `--turn` and omit `--disposition` for execution
outside a Turn. Turn dispositions are `failed` or `interrupted`; recovery cannot
mark uncertain work successful. The CLI never selects a Workspace version or
creates a reconciliation record on the operator's behalf.
