---
name: helmr-runtime
description: Delegate work and observe or control Helmr Sessions and Turns through the runtime MCP tools. Use when coordinating Agents, continuing work, checking outcomes, or interrupting and resuming owned work.
---

# Use the Helmr runtime

A Session retains an Agent's execution context across Turns. A Turn is one
accepted input and its eventual outcome. Use the tools exposed by this MCP
connection; the connection identifies your calling Session. Tool names below
may have a server prefix in your harness.

Follow the user's requested scope and delegation or approval requirements.
These instructions explain operations; they do not authorize creating,
interrupting, or cancelling work.

## Choose where work belongs

- For a new task you own, use `spawn`. It creates a child Session using your
  pinned Deployment. Your ownership lets you control that child's work.
- For a separate task that should be independent of you, use `start`. It uses
  the Environment's current Deployment. Being its requester does not make you
  its owner; do not assume you can interrupt or cancel it.
- Only root Sessions can `list_agents`, `spawn`, or `start`. If you are a child
  and need more delegation or a human decision, report that need to your owner.
- Call `list_agents` with the intended `operation` (`spawn` or `start`) to
  discover valid Agent IDs. Follow its cursor when needed; do not invent IDs.
- Omit `computerId` to create a Computer from the target Agent's definition.
  To deliberately share an existing Computer, use its exact ID; use
  `list_computers` when you need to discover one. Shared files and processes
  require coordination between the workloads.

For `spawn`, `start`, and `enqueue`, input is an array of text parts, for example
`[{"type":"text","text":"Review the change and return your findings."}]`.
If passing structured data, serialize it into text agreed with the target.
Retain the returned `sessionId`, `turnId`, and `sequence`.

## Continue or steer existing work

Use `enqueue` to admit another Turn in an exact Session. Existing holds remain
in force; enqueueing does not resume a held Session.

Use `send_turn` to deliver a message to an owned descendant's exact active Turn.
Its receipt confirms admission, not delivery to the handler. If it returns
`message_closed`, the Turn is not accepting messages: it may have settled, be
held, or have no registered message handler. Reinspect its state. Enqueue new
work only if the user's intent calls for a separate Turn.

If you lost a Session identity, `list_sessions` can recover direct children
(`relation: "owned"`) or work you requested, including independent work
(`relation: "requested"`). Results include initial Turn identities, not the
latest Turn. Follow `next_cursor`; omit `status` to include all states, including
terminal Sessions.

## Observe completion

A creation or enqueue receipt means work was accepted, not completed. Wait on
the exact returned Session and Turn with `wait_turn`; `timeoutMs` is from 1 to
30000. A `status: "timeout"` result only ends that observation. It neither
cancels the Turn nor permits submitting the same work again as a new task.
Repeat the wait while the task still calls for observation.

If `wait_turn` fails with `capacity_wait_blocked`, the target needs the Computer
allocation your Session holds. Do not keep waiting or submit duplicate work.
Finish your Turn and report the pending Session and Turn so work can continue
after the allocation is released.

A `status: "settled"` result contains `outcome`. Check its status before using
its result: failed, interrupted, or cancelled work is not successful output.
Use `inspect_turn` or `inspect_session` for current state. Report an unresolved
outcome as unresolved; a lost connection is not proof of completion or failure.

## Recover an uncertain mutation

Supply an `idempotencyKey` for every mutation. Keep one key for one logical
operation and reuse it with identical arguments if a response is lost. Use a
new key for genuinely new work, not to bypass an uncertain result.

A disconnect or timeout does not undo effects. Reconcile known Session and
Turn identities first; retry the same mutation with the same key when needed.
Do not blindly repeat an interrupted Turn: it may already have produced
external effects. Permission or lifecycle errors call for checking state or
reporting the limitation, not switching identities or repeatedly creating work.

## Interrupt, resume, drain, or cancel

These controls operate on owned descendants; an accepted control receipt is
not proof that execution has stopped.

- `interrupt_turn` targets one exact running Turn. It creates no Session hold
  and does not stop children. Queued or settled Turns cannot be
  interrupted this way. Confirm the target's outcome afterward.
- `interrupt_session` places a hold and requests interruption of the Session
  and its subtree. Retain the returned `holdId` and check state afterward.
- `resume_session` releases one exact hold on an owned descendant: post the
  hold's `id` to its `session_id`. `inspect_session` reports `can_release` for
  each hold. Only interrupts you placed can be released this way; other
  interrupts and storage pressure holds stay in force. Other holds can still prevent progress. It does not replay
  interrupted input.
- `close_session` closes admission while running and unblocked queued work
  drains. Queued work that a hold blocks and pending questions are cancelled.
  The result reports the Session status: `closing` or `closed`.
- `cancel_session` cancels pending work, requests cessation of active work in
  the subtree and closes it at once. The result reports status `closed`.
  Closing or cancelling a closed Session changes nothing.

All calls remain subject to the current Session's authority and admission
state. Reading this skill grants no additional access. The MCP transport is
separate from Helmr Session lifetime: disconnecting a client does not close or
cancel its work.
