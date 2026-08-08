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

For continuing questions, corrections, or commands, use an Actor and call
`session.input.receive()`. External clients append with
`client.sessions.ref(id).input.send(...)`. This preserves ordered history and
allows more than one interaction, but grants a broader channel capability than
a Token.

Both waits can park a Run durably. Set explicit timeouts and handle terminal
conditions instead of assuming a human will always respond.
