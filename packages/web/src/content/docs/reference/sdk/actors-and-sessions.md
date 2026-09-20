---
title: Actors, Sessions and Turns
description: Define Actor execution and operate explicit Turns within a durable Session.
---

# Actors, Sessions and Turns

`actor({ id, run, idleTimeout?, queue?, maxDuration?, ttl?, retry? })` defines
execution. Inputs, messages, outputs and results are application-defined JSON.
Validate with Zod or another library in your code where needed; Actor definitions
do not contain schemas or automatically infer external client payload types.

```ts
import { actor } from "@helmr/sdk"

export const reviewer = actor({
  id: "reviewer",
  async run(session) {
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      await turn.output.write({ received: turn.input })
      await turn.complete()
    }
  },
})
```

`actor.start({ workspace, key?, idempotencyKey?, run?, signal? })` returns
`{ session, run }`. Start does not accept initial input; enqueue work afterwards.
One Run can process multiple Turns. A Run is execution, a Turn is one queued unit
of work, and the Session is their stable interaction address.

## Inside an Actor

- `session.receive({ timeout?, idleTimeout?, metadata?, tags? })` returns a Turn,
  or `null` after a closing Session has drained. Timeout rejects; it is not a
  successful empty receive. The Actor idle timeout controls the warm wait before
  managed suspension, not the Session lifetime.
- A Turn has `id`, `sequence`, `input`, `source`, `createdAt`, and `signal`.
- `await turn.onMessage(handler)` registers one sequential handler and acknowledges
  readiness. Each callback receives `{ id, data }`; application fields such as
  `data.type` are entirely application-defined. Await readiness before publishing
  a question or approval request. Throw `MessageRejected` for a known rejection;
  an unexpected handler failure produces an unknown message outcome.
- `turn.output.write(value, { idempotencyKey? })` appends durable output associated
  with the Turn. `turn.output.pipe(iterable)` consumes a finite stream. Neither
  operation completes the Turn. `session.output.write/pipe` publishes explicit
  Session-level output outside Turn ownership.
- `await turn.complete(result?)` or `await turn.fail(error)` explicitly settles
  the Turn after started output and admitted callbacks drain. No-result completion omits the result;
  completing with `null` records JSON null. Returning from `run`, requesting the
  next Turn, or reaching stream EOF is not implicit completion.

Keep post-processing and deterministic tests inside the Turn before completion.
Use `turn.signal` for Turn-owned cancellation, and retain native provider SDK
behavior in application code. Helmr does not supply an `onTurn` shortcut, an
agent adapter requirement, or a dedicated ask/approval primitive.

## Outside an Actor

Use `client.actors.start(declaredId, request)` or `client.sessions.ref(id)`:

```ts
const session = client.sessions.ref(sessionId)
const turn = await session.enqueue({ issue: "APP-42" }, {
  idempotencyKey: "linear:event-123",
})
// In a later request, once this Turn is active:
await turn.send({ type: "update_constraints", allowDependencyChanges: false })
const state = await turn.retrieve()
const page = await session.events.list({ after: 0, limit: 100 })
```

`enqueue` returns a `TurnRef` and always creates FIFO work. `send` returns
`{ kind: "enqueued", turn }` or `{ kind: "messaged", turn, message }`: it sends
to the active Turn before settlement, even before its handler is registered, or
joins the FIFO queue when idle. Accepted messages retain that exact target and
are delivered when its handler is ready; they never fall back to a different Turn.
`session.turn(id).send(data)` always targets that exact active Turn and returns
a message receipt, not proof of application or provider handling. Held Sessions
reject `send`; `enqueue` can still add work to an open held Session.

`events.list({ after?, limit? })` returns `records`, `nextAfter`, `hasMore`, and
`retainedAfter`. All output and lifecycle events share one sequence. Records carry
`kind`, `data`, nullable `turnId`, and nullable Run provenance. Keep a cursor per
consumer, and deduplicate remote effects using event IDs. A finite page is not a
completion signal: inspect terminal Turn events or `turn.retrieve()`.

`turn.interrupt()` returns a stop receipt and hold identity. Acceptance may precede
physical convergence. Queued work stays retained. `session.resume({ holdId })`
releases that exact converged hold; it never replays the interrupted Turn.
`session.cancel()` rejects new input, records queued Turns as `cancelled`, and
requests interruption of active work. It returns an acceptance receipt; poll
`session.retrieve()` for `status: "closed"` before deleting its Workspace.
`cancelRequestedAt` remains visible on the Session. An active Turn uses the normal
`interrupted` outcome after its stop and capture complete. Previously completed
Turns and output remain unchanged.

```ts
await session.cancel({ idempotencyKey: "stop-review" })
const state = await session.retrieve()
// An accepted request does not prove physical termination.
if (state.status === "closed") {
  await client.workspaces.ref(state.workspaceId).delete()
}
```

Cancellation can escalate a Session already closing. It cannot be undone with
`resume()`. If execution or external effects are uncertain, the Session stays
held and requires the existing privileged `recover()` operation. Closure never
releases Workspace ownership until the old writer is excluded. Cancellation
also works through a runtime Session reference inside an Actor or Task.

`session.close()` rejects new ordinary admission and drains accepted FIFO work.
Existing holds survive closing, and exact interaction with active work may remain
valid. Session statuses are `open`, `closing`, `closed`, and `failed`; a failed Turn
does not by itself fail its Session.

Only authenticated client references expose `recover`: owner/admin plus the
recovery grant must supply an exact hold, Turn or explicit null, reconciled
Workspace version and reconciliation reference. Recovery cannot declare uncertain
work successful; it leaves a recovered hold for explicit resume. Runtime references
have ordinary controls but no recovery privilege.

Message acceptance does not wait for handler registration. During a managed Token
or child wait, messages remain pending until the original wait resumes; they do
not unblock that wait. Settlement rejects new sends with `turn_settling` and emits
rejections for accepted messages whose handlers never started. Acceptance is not
proof of application or provider handling. Held Sessions still require explicit resume.
