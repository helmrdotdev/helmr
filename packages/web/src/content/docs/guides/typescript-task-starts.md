---
title: TypeScript task starts
description: Start and observe Helmr task sessions from TypeScript.
section: Guides
sidebarLabel: Task starts
order: 370
---

# TypeScript task starts

Use the SDK from external TypeScript code when another service should start a Helmr task session. The task id must already exist in a deployment task source.

```ts
import { HelmrClient } from "@helmr/sdk"
import type { reviewPullRequest } from "./tasks/review-pull-request"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL,
  apiKey: process.env.HELMR_API_KEY,
})

const started = await client.tasks.start<typeof reviewPullRequest>(
  "review-pull-request",
  { owner: "OWNER", repo: "REPO", prNumber: 42 },
  {
    externalId: "github:OWNER/REPO#42",
    idempotencyKey: "github:delivery-id",
    idempotencyKeyTTL: "24h",
  },
)
```

`client.tasks.start()` starts or reuses a task session by task id. Imported task definitions can use `task.start()` when local payload schema validation is needed before posting. `externalId` identifies the durable session; `idempotencyKey` identifies one retry-safe start request. Retrieve or wait on the session, and use the returned run handle for compute/debug views:

```ts
const session = await client.sessions.wait(started.session, {
  timeoutSeconds: 10 * 60,
})

const currentRun = await client.runs.retrieve(started.run)
const logs = await client.runs.logs.retrieve(started.run)
const events = await client.runs.events.list(started.run)
```

Follow-up user messages, webhooks, or operator replies are session input, not task start payload:

```ts
await client.sessions.open(started.session).input("approval").send(
  { approved: true },
  { correlationId: "github:OWNER/REPO#42" },
)

for await (const record of await client.sessions.open(started.session).output("agent.report").stream()) {
  console.log(record.sequence, record.data)
}
```

## Completing waitpoint tokens

Waitpoint tokens are the external completion primitive. Task code creates a token and waits for it:

```ts
const token = await wait.createToken({ timeout: "1h" })
await sendReviewEmail({
  tokenId: token.id,
})

const decision = await wait.forToken(token, { schema: approvalSchema }).unwrap()
```

A service that receives the external action completes the token:

```ts
await wait.completeToken(token.id, {
  approved: true,
  actor: "email:reviewer@example.com",
})
```

The same completion route can be called without a Helmr API key when the caller has the token's `publicAccessToken`:

```ts
await fetch(`${process.env.HELMR_API_URL}/api/waitpoints/tokens/${token.id}/complete`, {
  method: "POST",
  headers: {
    authorization: `Bearer ${token.publicAccessToken}`,
    "content-type": "application/json",
  },
  body: JSON.stringify({ data: { approved: true } }),
})
```

Keep the public access token scoped to the external action that should be able to resume the session.

The client also reads `HELMR_API_URL` and `HELMR_API_KEY` from the environment when options are omitted. Authenticated SDK calls require an API key. Plain HTTP is accepted only for loopback hosts.

Task start payload is persisted as audit data. Keep credentials out of payload and declare task secrets in task source.
