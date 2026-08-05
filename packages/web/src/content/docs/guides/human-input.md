---
title: Human input
description: Continue an Actor with follow-up input or wait for one approval value.
section: Guides
sidebarLabel: Human input
order: 330
---

# Human input

Use an Actor when human input is part of a continuing conversation or
workflow. Use a Token when one outside action completes one waiting value.

## Continuing interaction

An Actor receives both its initial and later inputs through the same fixed
durable channel:

```ts
import { actor } from "@helmr/sdk"

export const reviewer = actor({
  id: "reviewer",
  async run(self) {
    const input = await self.input.receive({ idleTimeout: "30m" }).unwrap()
    await self.output.append({ type: "received", input })
  },
})
```

Send follow-up input with an idempotency key derived from the upstream event:

```ts
await sessions.ref(sessionId).input.send(
  { type: "instruction", text: "Please also update the tests." },
  { idempotencyKey: "slack:T123:C456:1712345678.000100" },
)
```

Use tagged unions in Actor code when several logical message kinds share the
channel. Helmr preserves ordering and record metadata; the deployed Actor owns
business validation.

## One-shot approval

For an email link, provider callback, or browser approval button, create a
Token inside the owning Run:

```ts
const approval = await tokens.create({
  timeout: "30m",
  metadata: { action: "publish-review" },
})

await sendApprovalEmail({ callbackUrl: approval.callbackUrl })
const decision = await approval.wait({ schema: approvalSchema }).unwrap()
```

The callback URL or returned public access credential authorizes completion of
that Token only. Retrying the same canonical completion replays; different
completion data conflicts and never overwrites the accepted value.
