---
title: Wait for human input
description: Use a Token for one external decision or Actor input for a conversation.
---

# Wait for human input

Choose the primitive by capability: Actor input continues a durable channel;
a Token grants one narrow completion for one value.

For an approval link or provider callback, create and wait on a Token inside
the owning Run:

```ts
import { tokens } from "@helmr/sdk"

const approval = await tokens.create({
  timeout: "30m",
  metadata: { action: "publish-review" },
  tags: ["approval"],
  idempotencyKey: `approval:${ctx.run.id}`,
})

await sendApprovalLink(approval.callbackUrl)
const decision = await approval.wait({
  timeout: "35m",
  schema: approvalSchema,
}).unwrap()
```

The create response is the public SDK result that includes `callbackUrl` and
`publicAccessToken`. Treat both as credentials. Completion makes the Token
`completed`; cancellation and expiry reject the wait with typed errors.

For continuing questions, corrections or commands, receive work with
`session.receive()` and register `await turn.onMessage(handler)` before publishing
a question. External callers use `client.sessions.ref(id).turn(turnId).send(data)`
for exact replies. Input `type`, request IDs and answer/approval payloads belong
to your application; Helmr does not prescribe a question or permission schema.

The generic Token SDK is also available inside an Actor. Completing a Token does
not complete the Turn or grant permission to perform a stopped action. For a live
native approval, bind the answer to the exact request/action and await a fenced
`turn.output.write({ type: "permission_admitted", requestId, actionBinding })`
before returning allow, only while that callback is still live. A rejected or
ambiguous write, historical receipt, or abort signal alone cannot authorize it.
Helmr treats this output as opaque application data.

Managed Token waits can park the Run. Native provider callbacks and arbitrary
sockets are not automatically durable; use their native cancellation contracts.
Set timeouts, reject late replies, and explicitly complete/fail the Turn only
after the application work has settled.
