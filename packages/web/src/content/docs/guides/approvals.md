---
title: Human input
description: Pause tasks for operator decisions or message input.
section: Guides
sidebarLabel: Human input
order: 330
---

# Human input

Use waitpoint tokens before side effects such as posting to GitHub, deploying,
or changing infrastructure. Helmr creates the durable waitpoint; your Slack,
email, Linear, or app-server bridge delivers the token and completes it.

```ts
import { task, wait } from "@helmr/sdk"

export const publishReview = task({
  id: "publish-review",
  run: async () => {
    const token = await wait.createToken({
      timeout: "30m",
      tags: ["approval", "github"],
      metadata: { action: "publish-review" },
    })

    await sendSlackApproval({
      tokenId: token.id,
    })

    const decision = await wait.forToken(token, {
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
await wait.completeToken(token.id, {
  approved: true,
  actor: "slack:U123",
})
```

Browser or raw HTTP action handlers can complete the same token with
`Authorization: Bearer ${token.publicAccessToken}` on
`/api/waitpoints/tokens/{tokenId}/complete`. Server-to-server webhook handlers
can use `token.callbackUrl` when a pre-signed completion URL is a better fit.

The CLI exposes the same primitive:

```sh
helmr waitpoint list \
  --project PROJECT_ID \
  --env ENV_ID
helmr waitpoint token complete TOKEN_ID \
  --data '{"approved":true}'
```

Only one blocking waitpoint or time wait can be active at a time in a task.
Await each wait before starting the next one.
