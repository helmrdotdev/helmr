---
title: HelmrClient
description: Authenticated TypeScript client for Agents, Sessions, Turns and Computers.
---

# HelmrClient

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({
  url,
  apiKey: process.env.HELMR_API_KEY!,
})
```

`apiKey` is required. Set `url` explicitly to your control plane for self-hosting.
The SDK defaults an omitted URL to `https://api.helmr.dev`; it does not read
`HELMR_API_URL` itself. The example rejects missing configuration before sending
credentials. A custom `fetch` may be supplied.
Transport methods accept an optional `{ signal }` options object; Turn waits
accept `{ signal, timeout }`. Explicit client calls use the
supplied credentials even when constructed inside Agent code.

| Property | Operations |
| --- | --- |
| `agents` | `retrieve`, `list`, `start` |
| `sessions` | `retrieve`, `list`, `get` |
| `computerDefinitions` | `retrieve`, `list`, `createComputer` |
| `computers` | `retrieve`, `list`, `ref` |
| `commands` | `ref` |
| `deployments` | `list`, `current`, `retrieve` |
| `schedules` | `retrieve`, `list` |
| `secrets` | `create`, `retrieve`, `list`, `ref` |
| `slack` | `channels.list`, `channels.get` |

Resource list methods return `{ items, nextCursor? }`; Slack channel listing uses
`{ channels, nextCursor? }`. Definition pages also identify their Deployment. Treat cursors as opaque. A Session reference exposes Turn admission,
controls, events and exact Turn references. A Turn reference provides outcome
observation, steering messages and ask responses.

```ts
const { session, turn } = await client.agents.start("review", {
  input: [{ type: "text", text: "Review issue 42" }],
  idempotencyKey: "review:42",
})
const waiting = turn.wait({ timeout: "5m" })
const progress = await session.events.list({ after: 0 })
const observation = await waiting
if (observation.status === "settled") {
  console.log(observation.outcome)
}
const next = await session.enqueue([{ type: "text", text: "Review issue 43" }])
console.log(await next.wait())
```

A timed wait can return `{ status: "timeout" }` without stopping the work.
Process stdout/stderr and preparation diagnostics are internal; application
content is available through Session events associated with its Turn.
