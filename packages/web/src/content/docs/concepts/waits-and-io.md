---
title: Waits and session I/O
description: Choose between session streams, external callback tokens, and timers.
section: Concepts
sidebarLabel: Waits and I/O
order: 170
---

# Waits and session I/O

Helmr has four public surfaces for session interaction and waits:

| Use case | Primitive | Why |
| --- | --- | --- |
| Human messages, approvals, corrections, webhook replies, cancel buttons | Session input stream | The input belongs to the session transcript and may be followed by more input later. |
| Structured output for clients or operator tools | Session output stream | Consumers can read durable task output without parsing logs. |
| Email links, third party callbacks, or one-shot bridge completions | Token | The outside system receives a scoped completion capability instead of a session stream address. |
| Sleep until a duration or timestamp | Timer | The condition is time, not outside input. |

When a task calls a blocking `.wait()` API and the condition is not already
satisfied, Helmr parks the current run with an internal wait record. The run
resumes when matching stream input arrives, a token is completed, or the timer
expires. These are peer wait types in Helmr; input streams are not modeled as
tokens.

Only one blocking wait can be active in a task execution at a time. Await the
current wait before starting the next one.

## Session Input

Use input streams for human-in-the-loop agent sessions. They keep follow-up
messages, operator decisions, webhook replies, and corrections attached to the
session.

```ts
import { streams } from "@helmr/sdk"
import { z } from "zod"

const messages = streams.input("messages", {
  schema: z.object({
    text: z.string(),
    actor: z.string(),
  }),
})

const nextMessage = await messages.wait({
  timeout: "30m",
  correlationId: "thread-1",
}).unwrap()
```

Backends and operator tools append input through session handles:

```ts
await client.sessions.open(sessionId).input(messages.id).send(
  {
    text: "Please also update the tests.",
    actor: "slack:U123",
  },
  {
    correlationId: "thread-1",
  },
)
```

Use `.wait()` for long waits that should release compute. Use `once()` or
`on(...)` only while the task should stay active and consume active runtime.
Use `peek()` when the task should inspect buffered input without consuming it.

## Session Output

Use output streams when clients need structured records from the task. Output
streams are durable session history, not logs.

```ts
const events = streams.output("agent.events", {
  schema: z.object({
    type: z.string(),
    message: z.string(),
  }),
})

await events.append({
  type: "review.started",
  message: "Reviewing pull request.",
})
```

Clients can list or read output records from a cursor:

```ts
const records = await client.sessions.open(sessionId).output(events.id).list()
```

## External Callback Tokens

Use tokens when the outside world should complete a one-shot capability rather
than append to a session stream. Common cases are email links, provider callback
URLs, and bridge services that should not receive the session id and stream
name.

```ts
import { tokens } from "@helmr/sdk"

const token = await tokens.create({
  timeout: "1h",
  tags: ["approval", "email"],
  metadata: { action: "release" },
})

await sendApprovalEmail({
  callbackUrl: token.callbackUrl,
})

const decision = await token.wait({
  schema: approvalDecisionSchema,
}).unwrap()
```

Backend code and the CLI complete tokens with a Helmr API key or session:

```ts
await client.tokens.complete(token.id, {
  approved: true,
  reviewer: "email:reviewer@example.com",
})
```

Browser or raw HTTP flows can complete the same token with the token's scoped
public access token:

```ts
await fetch(`/api/v1/tokens/${token.id}/complete`, {
  method: "POST",
  headers: {
    authorization: `Bearer ${token.publicAccessToken}`,
    "content-type": "application/json",
  },
  body: JSON.stringify({ data: { approved: true } }),
})
```

Server-to-server integrations can use `token.callbackUrl` as a pre-signed
completion URL. The callback URL contains a single-token secret in the path and
is intended for webhook providers, not browser UI. `publicAccessToken` and
`callbackUrl` are returned only when a token is created; retrieve and list
responses do not expose completion secrets again.

Completing a token is idempotent when the completion `data` is the same. If the
same token is completed again with the same canonical `data`, Helmr returns the
first successful completion; a different `data` value is rejected as a conflict.

## Timers

Use time waits when the task should resume after a duration or timestamp:

```ts
await timers.waitFor("10m")
await timers.waitUntil(new Date("2026-06-01T00:00:00Z"))
```

Timers are the right choice for backoff, delayed follow-up, scheduled polling,
and timeboxed agent steps where no external data is needed to resume.

## Checkpoints

When a worker parks a run, Helmr durably stores the checkpoint and resume state
needed to continue execution. The task process resumes with filesystem, memory,
and the run context restored by the worker runtime.
