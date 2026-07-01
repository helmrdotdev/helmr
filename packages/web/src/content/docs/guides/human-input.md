---
title: Human input
description: Pause an agent session until a user, operator, webhook, or bridge sends more input.
section: Guides
sidebarLabel: Human input
order: 330
---

# Human input

Use session input when an agent run needs a human decision, follow-up message,
webhook reply, or operator correction. The task waits on a named input stream.
Helmr parks the run durably, keeps the session history attached, and resumes
the run when matching input arrives.

```ts
import { streams, task } from "@helmr/sdk"
import { z } from "zod"

const approval = streams.input("approval", {
  schema: z.object({
    decision: z.enum(["approve", "reject", "edit"]),
    note: z.string().optional(),
  }),
})

export const publishReview = task({
  id: "publish-review",
  run: async (event, ctx) => {
    await sendSlackApproval({
      sessionId: ctx.session.id,
      stream: approval.id,
      correlationId: `github:${event.owner}/${event.repo}#${event.prNumber}`,
    })

    const decision = await approval.wait({
      timeout: "30m",
      correlationId: `github:${event.owner}/${event.repo}#${event.prNumber}`,
    }).unwrap()

    if (decision.decision !== "approve") return { status: "skipped" }
    await postReview()
    return { status: "posted" }
  },
})
```

Send input from a trusted app server, webhook handler, or operator tool:

```ts
await client.sessions.open(sessionId).input(approval.id).send(
  {
    decision: "approve",
    note: "Looks good.",
  },
  {
    correlationId: `github:${owner}/${repo}#${prNumber}`,
  },
)
```

Use `correlationId` when the same stream can carry more than one pending
decision for a session. The waiting run only resumes from input that matches the
wait's stream and correlation id.

## Browser Actions

For browser UI, create a scoped public access token that can only append to one
session input stream. The browser receives the opaque token, not a Helmr API key:

```ts
import { auth } from "@helmr/sdk"

const inputToken = await auth.createPublicToken({
  scope: {
    type: "session.input.send",
    session: sessionId,
    stream: approval,
    correlationId: `github:${owner}/${repo}#${prNumber}`,
  },
  maxUses: 1,
})
```

The browser or action endpoint can then send the input with that bearer token:

```ts
await client.sessions.open(sessionId).input(approval.id).send(
  { decision: "approve" },
  {
    publicAccessToken: inputToken.publicAccessToken,
    correlationId: `github:${owner}/${repo}#${prNumber}`,
  },
)
```

## External Callbacks

Use an externally completable token when the integration is naturally a callback
target instead of a session input surface: for example, an email link, a third
party webhook provider, or a bridge that should not know the session id and
stream name.

```ts
import { tokens } from "@helmr/sdk"

const token = await tokens.create({
  timeout: "30m",
  tags: ["approval", "email"],
  metadata: { action: "publish-review" },
})

await sendApprovalEmail({
  callbackUrl: token.callbackUrl,
})

const decision = await token.wait({
  schema: approvalDecisionSchema,
}).unwrap()
```

Server-side bridge code can complete the token with an API key:

```ts
await client.tokens.complete(token.id, {
  approved: true,
  actor: "email:reviewer@example.com",
})
```

Prefer session input when the response belongs in the agent session transcript.
Prefer tokens when you need a one-shot callback capability that can be completed
without exposing a session stream.

## Inspecting Waits

Operators can inspect the relevant session and run attempts from the CLI:

```sh
helmr session get SESSION_ID
helmr run list --session SESSION_ID
```

Only one blocking token, stream, or timer wait can be active at a time in a task.
Await each wait before starting the next one.
