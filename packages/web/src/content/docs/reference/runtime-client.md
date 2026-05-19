---
title: Runtime client
description: TypeScript client APIs for triggering and inspecting Helmr runs.
section: Reference
sidebarLabel: Runtime client
order: 920
---

# Runtime client

`HelmrClient` provides the TypeScript runtime API:

```ts
import { HelmrClient, workspace } from "@helmr/sdk"

const client = new HelmrClient({
  url: process.env.HELMR_URL,
  apiKey: process.env.HELMR_API_KEY,
})
```

Authenticated calls require an API key. Delegated token completion can run without one. `http://` is allowed only for loopback hosts.

Main surfaces:

| API | Purpose |
| --- | --- |
| `client.tasks.trigger(task, opts)` | Create a run for a typed task. |
| `client.runs.retrieve(run)` | Fetch current run snapshot. |
| `client.runs.wait(run, opts)` | Poll until terminal status. |
| `client.runs.list(opts)` | List run summaries. |
| `client.runs.logs.retrieve(run)` | Read latest stdout/stderr snapshot. |
| `client.runs.events.list(run, opts)` | Page through run events. |
| `client.runs.events.subscribe(run, opts)` | Stream events with SSE. |
| `client.waitpoints.approve`, `deny`, `reply` | Resolve pending waitpoints. |
| `client.waitpoints.tokens.create(waitpoint, opts)` | Create an expiring delegated waitpoint response token. |
| `client.waitpoints.tokens.complete(token, opts)` | Complete a delegated waitpoint response token. |

Payload is persisted as audit data in the control plane. Put secret values in declared `secrets`, not in payload.
