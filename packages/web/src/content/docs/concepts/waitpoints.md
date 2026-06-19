---
title: Waitpoints
description: Durable pauses inside a running task.
section: Concepts
sidebarLabel: Waitpoints
order: 170
---

# Waitpoints

Waitpoints pause a run while it waits for time to pass or for an external
completion. Time-based pauses use `wait.for` and `wait.until`. External
completion uses waitpoint tokens.

```ts
import { task, wait } from "@helmr/sdk"

export const release = task({
  id: "release",
  run: async () => {
    const token = await wait.createToken({
      timeout: "1h",
      tags: ["approval", "production"],
      metadata: { release: "2026.06.15" },
    })

    await sendSlackApproval({
      tokenId: token.id,
    })

    const decision = await wait.forToken(token, {
      schema: approvalDecisionSchema,
    }).unwrap()

    if (!decision.approved) return { deployed: false }
    return await deployProduction()
  },
})
```

## Waitpoint Tokens

A waitpoint token is a scoped capability that can unblock one waiting run.
Userland Slack apps, email flows, Linear webhooks, custom dashboards, and app
servers deliver the token and complete it.

```ts
await wait.completeToken(token.id, {
  approved: true,
  reviewer: "slack:U123",
})
```

Backend code and the CLI complete tokens with a Helmr API key or session. Browser
or raw HTTP flows can complete the same endpoint with the token's public access
token:

```ts
await fetch(`/api/waitpoints/tokens/${token.id}/complete`, {
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

Use the CLI to inspect waitpoints and manage tokens:

```sh
helmr waitpoint list \
  --project PROJECT_ID \
  --env ENV_ID
helmr waitpoint token create \
  --project PROJECT_ID \
  --env ENV_ID \
  --timeout-seconds 3600
helmr waitpoint token complete TOKEN_ID \
  --data '{"approved":true}'
```

## Time Waits

Use time waits when a task should resume after a duration or timestamp:

```ts
await wait.for("10m")
await wait.until(new Date("2026-06-01T00:00:00Z"))
```

## Checkpoints

When a worker creates a waitpoint, Helmr durably stores the checkpoint and
resume state needed to continue the run. Only one blocking wait operation can be
active in an execution at a time.
