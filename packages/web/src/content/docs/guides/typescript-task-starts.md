---
title: TypeScript session starts
description: Start and observe Helmr sessions from TypeScript.
section: Guides
sidebarLabel: Session starts
order: 370
---

# TypeScript session starts

Use the SDK from external TypeScript code when another service should start a Helmr session. The selected task, whether passed as an imported task definition or a string id, must already exist in a deployment task source.

```ts
import { runs, sessions } from "@helmr/sdk"
import { reviewPullRequest } from "./tasks/review-pull-request"

const reviewPayload = { owner: "OWNER", repo: "REPO", prNumber: 42 }

const started = await sessions.start(
  reviewPullRequest,
  reviewPayload,
  {
    externalId: "github:OWNER/REPO#42",
    idempotencyKey: "github:delivery-id",
    idempotencyKeyTTL: "24h",
  },
)
```

`sessions.start()` / `client.sessions.start()` and `sessions.startAndWait()` / `client.sessions.startAndWait()` are the canonical APIs for starting or reusing a session. Use the top-level `sessions` facade with `HELMR_API_URL` and `HELMR_API_KEY`; use `new HelmrClient(...)` when the caller needs explicit credentials or multiple control-plane targets. `task(...)` returns a definition object only; pass that task object to the sessions namespace for payload input, output, and secrets type inference plus local payload schema validation. Pass a string task id when the caller is at an external boundary or the task id is dynamic. `externalId` identifies the durable session; `idempotencyKey` identifies one retry-safe start request. Use `startAndWait()` when the caller needs the first run's terminal output; use the returned run handle for compute/debug views:

```ts
const completed = await sessions.startAndWait(
  reviewPullRequest,
  reviewPayload,
  {
    timeoutSeconds: 10 * 60,
  },
)

const currentRun = await runs.retrieve(started.run)
const logs = await runs.logs.retrieve(started.run)
const events = await runs.events.list(started.run)
```

Follow-up user messages, webhooks, or operator replies are session input, not session start payload:

```ts
await sessions.open(started.session).input("approval").send(
  { approved: true },
  { correlationId: "github:OWNER/REPO#42" },
)

const reportRecords = await sessions.open(started.session).output("agent.report").list()
for (const record of reportRecords) {
  console.log(record.sequence, record.data)
}
```

## Completing tokens

Tokens are the external completion primitive. Task code creates a token and waits for it:

```ts
const token = await tokens.create({ timeout: "1h" })
await sendReviewEmail({
  tokenId: token.id,
})

const decision = await token.wait({ schema: approvalSchema }).unwrap()
```

A service that receives the external action completes the token:

```ts
await client.tokens.complete(token.id, {
  approved: true,
  actor: "email:reviewer@example.com",
})
```

The same completion route can be called without a Helmr API key when the caller has the token's `publicAccessToken`:

```ts
await fetch(`${process.env.HELMR_API_URL}/api/v1/tokens/${token.id}/complete`, {
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

Session start payload is persisted as audit data. Keep credentials out of payload and declare task secrets in task source.
