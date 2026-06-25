---
title: Streams, Tokens, And Timers
description: Durable session I/O, external completion, and long waits.
section: Concepts
sidebarLabel: Streams, Tokens, Timers
order: 170
---

# Streams, Tokens, And Timers

Streams, tokens, and timers are the public primitives for pausing or interacting
with task sessions. When a task uses a `.wait()` API and the condition is not
already satisfied, Helmr parks the current run with an internal `run_waits`
record and resumes it when the matching stream input, token completion, or
timer is ready.

Input streams carry durable session input. Task code can park on input with
`stream.wait(...)`, inspect buffered input with `peek()`, or stay active with
`once()` / `on(...)`. Backends and browser flows send input through explicit
session handles. Use `.wait()` for long waits; use `once()` / `on(...)` only
while the run should remain active and consume active compute budget.

```ts
await client.sessions.open(sessionId).input("messages").send({
  text: "continue",
})
```

Output streams carry durable task output for clients to list or read.

```ts
await events.append({
  type: "started",
})
```

Tokens model externally completed values such as approvals, callbacks, or tool
results.

```ts
import { task, tokens } from "@helmr/sdk"

export const release = task({
  id: "release",
  run: async () => {
    const token = await tokens.create({
      timeout: "1h",
      tags: ["approval", "production"],
      metadata: { release: "2026.06.15" },
    })

    await sendSlackApproval({
      tokenId: token.id,
    })

    const decision = await token.wait({
      schema: approvalDecisionSchema,
    }).unwrap()

    if (!decision.approved) return { deployed: false }
    return await deployProduction()
  },
})
```

## Tokens

An externally completable token is a scoped capability that can unblock one waiting run.
Userland Slack apps, email flows, Linear webhooks, custom dashboards, and app
servers deliver the token and complete it.

```ts
await client.tokens.complete(token.id, {
  approved: true,
  reviewer: "slack:U123",
})
```

Backend code and the CLI complete tokens with a Helmr API key or session. Browser
or raw HTTP flows can complete the same endpoint with the token's public access
token:

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
responses do not expose completion secrets again. Helmr core does not send Slack
messages, send email, manage recipients, or render provider-specific decision
UI.

Completing a token is idempotent when the completion `data` is the same. If the
same token is completed again with the same canonical `data`, Helmr returns the
first successful completion; a different `data` value is rejected as a conflict.

## CLI

Use the CLI to inspect the session and its run attempts:

```sh
helmr session get SESSION_ID
helmr run list --session SESSION_ID
helmr run events RUN_ID
```

## Time Waits

Use time waits when a task should resume after a duration or timestamp:

```ts
await timers.waitFor("10m")
await timers.waitUntil(new Date("2026-06-01T00:00:00Z"))
```

## Checkpoints

When a worker parks a run, Helmr durably stores the internal checkpoint and
resume state needed to continue execution. Only one blocking wait operation can
be active in an execution at a time.
