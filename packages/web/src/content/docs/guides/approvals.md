---
title: Human input
description: Pause tasks for operator decisions or message input.
section: Guides
sidebarLabel: Human input
order: 330
---

# Human input

Use tokens before side effects such as posting to GitHub, deploying, or
changing infrastructure. Helmr parks the run durably; your Slack, email,
Linear, or app-server bridge delivers the token and completes it.

```ts
import { task, tokens } from "@helmr/sdk"

export const publishReview = task({
  id: "publish-review",
  run: async () => {
    const token = await tokens.create({
      timeout: "30m",
      tags: ["approval", "github"],
      metadata: { action: "publish-review" },
    })

    await sendSlackApproval({
      tokenId: token.id,
    })

    const decision = await token.wait({
      schema: approvalDecisionSchema,
    }).unwrap()

    if (!decision.approved) return { status: "skipped" }
    await postReview()
    return { status: "posted" }
  },
})
```

Complete the token from a userland bridge:

```ts
await client.tokens.complete(token.id, {
  approved: true,
  actor: "slack:U123",
})
```

Browser or raw HTTP action handlers can complete the same token with
`Authorization: Bearer ${token.publicAccessToken}` on
`/api/v1/tokens/{tokenId}/complete`. Server-to-server webhook handlers
can use `token.callbackUrl` when a pre-signed completion URL is a better fit.

Operators can inspect the relevant session and run attempts from the CLI:

```sh
helmr session get SESSION_ID
helmr run list --session SESSION_ID
```

Only one blocking token, stream, or timer wait can be active at a time in a task.
Await each wait before starting the next one.
