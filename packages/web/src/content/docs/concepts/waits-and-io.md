---
title: Waits and durable I/O
description: Choose between Actor channels, Tokens, and timers.
section: Concepts
sidebarLabel: Waits and I/O
order: 170
---

# Waits and durable I/O

Helmr has three durable interaction boundaries:

| Use case | Primitive |
| --- | --- |
| Follow-up messages, corrections, commands, and progressive application output | Actor input/output |
| One externally completed value such as an approval or callback | Token |
| Sleep until a duration or timestamp | Timer |

Each Actor has exactly two ordered append logs: one input channel and one
output channel. They are fixed, not named, and not separately declared.
Application protocols use ordinary tagged JSON values.

```ts
const input = await self.input.receive({ idleTimeout: "30m" }).unwrap()
await self.output.append({ type: "accepted", input })
```

External clients and other Runs send Actor input through `ActorRef.input`.
Actor output may have many independent readers:

```ts
await operator.ref({ key: "production" }).input.send(
  { type: "steer", instruction: "also update the tests" },
  { idempotencyKey: "slack:thread-1:message-7" },
)

const records = await operator.ref({ key: "production" }).output.list()
```

Actor input may durably suspend the current managed Run. Only that Actor's
current Run consumes and advances its input cursor. Accepted input can start a
continuation Run while preserving the Actor and its channel history.

Use a Token when the outside system should complete one value without gaining
an Actor channel capability:

```ts
const approval = await tokens.create({ timeout: "1h" })
await sendApproval({ callbackUrl: approval.callbackUrl })
const decision = await approval.wait({ schema: approvalSchema }).unwrap()
```

The creation response is the only response that reveals the Token's callback
URL and public access credential. Public access is limited to
`token.complete`; Actor-channel browser grants are deferred.

Timers suspend on time rather than external input. One Run owns at most one
active suspension/release condition at a time.
