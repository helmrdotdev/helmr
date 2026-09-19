# Session and Turn contract

This is the selected prerelease contract, replacing the earlier Session lifecycle
proposal. Implementation is incremental within one integration candidate: no old/new
compatibility API is shipped. See [the precise contract](contract.md) for storage,
ordering, envelopes and error behavior. Qualification results belong in
[validation](validation.md), including the distinction between transaction proof and
native execution proof.

Helmr runs customer code. Interfaces such as Slack, Linear, Discord and HTTP own
authentication, external identity mapping and presentation. Applications own their
provider SDKs, routing, permissions, questions and success criteria. Helmr owns
admission, execution/Workspace authority, managed waits and lifecycle outcomes. A
Helmr Turn need not equal a provider turn and can contain several SDK operations,
tests, human interactions and child Tasks.

## Authoring

Only `actor({ run(session, ctx) })` is provided. Initialization, loop and cleanup are
ordinary customer TypeScript. No `onTurn`, `onRequest` or return-to-complete wrapper.

```ts
const fixer = actor({
  id: "issue-fixer",
  async run(session, ctx) {
    const agent = await createApplicationAgent(ctx);
    try {
      for (;;) {
        const turn = await session.receive();
        if (turn === null) return;
        await turn.onMessage(({ data }) => agent.handleUpdate(data));
        // The application helper owns native correlation, cancellation and SDK use.
        await turn.output.pipe(agent.execute(turn.input, turn.signal));
        const checks = await runChecks(turn.signal);
        if (!checks.ok) {
          await turn.fail({ code: "checks_failed", details: checks });
          continue;
        }
        await turn.complete({ summary: checks.summary });
      }
    } finally {
      await agent.close();
    }
  },
});
```

This is an authoring example, not a runnable provider adapter. The application
helpers above are not Helmr APIs. Forced process loss cannot run `finally`.

`receive()` returns a Turn, with input, immutable ID, interruption signal, message
registration, output writer, complete and fail. Another receive while it is unresolved
fails. An empty drained closing Session returns null before Run ownership release.
Run return, output EOF and provider success never settle an active Turn. Deliberate
failure uses `fail`; uncertain exceptions/loss require recovery.

Run remains logical program execution. Multiple successful Turns can share a Run.
Initial interruption ends the owning Run/process group while preserving the Session
and queued inputs. Task Runs do not acquire mandatory Sessions or Turns.

## External operations

| Operation | Meaning |
| --- | --- |
| `actor.start(options)` | Returns `{session, run}`. Send/enqueue the first input separately. |
| `session.send(data, {idempotencyKey?})` | Atomically messages an active Turn before settlement, or joins the ordinary FIFO tail when idle. Never falls back after target selection. |
| `session.enqueue(data, {idempotencyKey?})` | Always queues independent work, including while an open Session is held. |
| `session.turn(id).send(data, {idempotencyKey?})` | Exact active-Turn message; never enqueues new work. |
| `session.turn(id).interrupt({idempotencyKey?})` | Accepts exact active-Turn stop and holds dispatch; receipt is not quiescence. |
| `session.turn(id).retrieve()` | Non-consuming state/result view. No blocking join. |
| `session.events.list({after?, limit?})` | One retained output/lifecycle timeline. |
| `session.close({idempotencyKey?})` | Rejects new ordinary work and drains accepted input, preserving holds. |
| `session.resume({holdId, idempotencyKey?})` | Releases exactly the observed, settled hold. |
| `session.recover({...})` | Privileged repair after writer exclusion and external reconciliation; then explicit resume. |

Envelope identity and dispositions are fixed. Payloads, including `type`, are free
application JSON. No Actor schema declarations or receive-time schema options are
provided. Customers can parse values with Zod or any library where they use them;
that validation does not automatically type external references. A known invalid
message can throw MessageRejected; an unexpected callback failure remains unknown.
Task payload and Token wait Standard Schema support are unchanged.

Input admission is independent from handler/worker readiness. Messages accepted
before registration or during a managed wait stay bound to the active Turn and
are delivered in order when that execution becomes ready. They do not resolve or
wake a Token/child wait; delivery resumes after the original wait resolves. Do not
make a consuming wait depend on its paused message handler. Holds reject send.
Settlement first rejects new sends with turn_settling; admission first may yield a
later message.rejected event if the callback never started. Neither case retargets.

## Output and results

Use `turn.output.write(data, {idempotencyKey?})` or await `turn.output.pipe(iterable)`.
The same writer exists explicitly at Session scope; it does not infer an active
Turn. No append/emit aliases, writer factory, close/flush call or contentType option.
Records are JSON; binary/large artifacts are referenced from retained storage.

Write acknowledges durable output and returns event identity/cursor/provenance.
Pipe awaits writes in order, applies backpressure and propagates errors. Neither
completes work. Complete optionally stores one final value; absence and JSON null
differ. Both output and result are application-defined JSON; their structure is
not declared on the Actor or inferred from the run return value.

One Session event cursor covers application output and runtime lifecycle. Application
data cannot forge terminal events. A terminal event, input disposition/cursor and
proven Workspace head are one transaction. Previously published output is not
rolled back when the Turn fails. Remote UI projection cannot be exactly-once merely
because it retains that cursor.

## Native interaction and generic waits

Use exact-Turn messages and an application pending-request registry to answer a
live native question/approval. Bind native request IDs and provider lifetime; register
before publishing output, reject duplicates/conflicts and invalidate on native
resolution, stop or terminal settlement. Keep reading the native transport while
several requests are pending. A nonblocking question does not park the whole Turn.

Common `tokens.create()` and Token `wait()` remain available inside Actors and Tasks.
There is no `turn.tokens`, `turn.ask` or mandatory action digest on generic Tokens.
Only wait associations acquire Turn/execution ownership. Stopping one Turn does not
cancel a shared global Token. A completed result is data, not a permission grant or
the restoration of a lost native callback. The single consuming-wait gate stays.

The [contract](contract.md#permission-admission) specifies the additional ordering
needed before a native allow response. A local AbortSignal or stored Token answer
alone does not order permission against a control-plane stop.
