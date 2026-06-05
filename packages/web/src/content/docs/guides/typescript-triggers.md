---
title: TypeScript triggers
description: Start and observe Helmr runs from TypeScript.
section: Guides
sidebarLabel: TypeScript triggers
order: 370
---

# TypeScript triggers

Use the SDK from external TypeScript code when another service should start Helmr runs. The task id must already exist in a deployment task source.

```ts
import { reviewPullRequest } from "./tasks/review-pull-request"

const handle = await reviewPullRequest.trigger(
  { owner: "OWNER", repo: "REPO", prNumber: 42 },
)
```

`task.trigger()` creates a run for the task id; it does not upload source. For tasks with `payload`, it validates the payload before posting the run. It returns a run handle. Retrieve, wait, inspect logs, or stream events from that handle:

```ts
import { HelmrClient } from "@helmr/sdk"
import type { reviewPullRequest } from "./tasks/review-pull-request"

const client = new HelmrClient({
  url: process.env.HELMR_URL,
  apiKey: process.env.HELMR_API_KEY,
})

const handle = await client.tasks.trigger<typeof reviewPullRequest>(
  "review-pull-request",
  { owner: "OWNER", repo: "REPO", prNumber: 42 },
)
```

Use the id-based form when the triggering service should avoid importing the task implementation at runtime. Retrieve, wait, inspect logs, or stream events from the returned handle:

```ts
const current = await client.runs.retrieve(handle)

if (current.pendingWaitpoint?.kind === "human") {
  await client.waitpoints.respond(current.pendingWaitpoint, {
    value: { approved: true },
  })
}

const finished = await client.runs.wait(handle, {
  timeoutMs: 10 * 60_000,
  intervalMs: 1_000,
})

const logs = await client.runs.logs.retrieve(handle)
const events = await client.runs.events.list(handle)
```

## Responding to waitpoints

Use `client.waitpoints.respond` from trusted server-side code that can hold a Helmr API key:

```ts
const current = await client.runs.retrieve(handle)

if (current.pendingWaitpoint?.kind === "human") {
  await client.waitpoints.respond(current.pendingWaitpoint, {
    value: { text: "continue" },
  })
}
```

For delegated response flows, create a scoped waitpoint response token from trusted code and send the token URL to the reviewer. The delegated responder does not need your Helmr API key.

```ts
const current = await client.runs.retrieve(handle)

if (current.pendingWaitpoint?.kind === "human") {
  const responseToken = await client.waitpoints.tokens.create(current.pendingWaitpoint, {
    expiresInSeconds: 60 * 60,
    metadata: { recipient: "reviewer@example.com" },
  })

  await sendReviewEmail({
    to: "reviewer@example.com",
    approveUrl: responseToken.url,
  })
}
```

A service that receives the delegated response can use the token without the run id or waitpoint id:

```ts
await client.waitpoints.tokens.respond(responseToken, {
  value: { approved: true },
  externalSubject: "reviewer@example.com",
  metadata: { source: "email" },
})

await client.waitpoints.tokens.respond(responseToken.id, responseToken.token, {
  value: { text: "Use the staging database" },
  externalSubject: "reviewer@example.com",
})
```

Use trusted SDK responses when your service owns the decision and can keep `HELMR_API_KEY` private. Use delegated tokens when a person or external system should respond through a narrow, expiring capability.

The client also reads `HELMR_URL` and `HELMR_API_KEY` from the environment when options are omitted. Authenticated calls require an API key. Delegated token responses can run without one. Plain HTTP is accepted only for loopback hosts.

Payload is persisted as audit data. Keep credentials out of payload and declare task secrets in task source.
