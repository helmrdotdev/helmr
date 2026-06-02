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

Authenticated calls require an API key. Delegated token responses can run without one. `http://` is allowed only for loopback hosts.

Main surfaces:

| API | Purpose |
| --- | --- |
| `task.trigger(payload, opts)` | Create a run for an imported task and validate `payload` before posting. |
| `client.tasks.trigger<typeof task>(id, payload, opts)` | Create a run by task id without importing the task implementation at runtime. |
| `client.runs.retrieve(run)` | Fetch current run snapshot. |
| `client.runs.wait(run, opts)` | Poll until terminal status. |
| `client.runs.list(opts)` | List run summaries. |
| `client.runs.logs.retrieve(run)` | Read latest stdout/stderr snapshot. |
| `client.runs.events.list(run, opts)` | Page through run events. |
| `client.runs.events.subscribe(run, opts)` | Stream events with SSE. |
| `client.schedules.create(opts)` | Create an imperative cron schedule for a deployed task. |
| `client.schedules.list(opts)` | List schedules in a project environment. |
| `client.schedules.retrieve(id, opts)` | Fetch one schedule. |
| `client.schedules.update(id, opts)` | Update an imperative schedule. |
| `client.schedules.activate(id, opts)` | Activate an imperative schedule. |
| `client.schedules.deactivate(id, opts)` | Deactivate an imperative schedule. |
| `client.schedules.delete(id, opts)` | Delete an imperative schedule. |
| `client.waitpoints.create(opts)` | Create a standalone manual waitpoint. |
| `client.waitpoints.respond(waitpoint, opts)` | Respond to a caller-resolvable manual waitpoint. |
| `client.waitpoints.tokens.create(waitpoint, opts)` | Create an expiring delegated waitpoint response token. |
| `client.waitpoints.tokens.respond(token, opts)` | Respond using a delegated waitpoint response token. |

Payload is persisted as audit data in the control plane. Put secret values in declared `secrets`, not in payload.

Schedules use cron and generated schedule metadata payloads. `client.schedules.create()` accepts `task`, `deduplicationKey`, `externalId`, `cron`, `timezone`, `workspace`, `secretBindings`, and run `options`. It does not accept arbitrary payload. Declarative schedules are defined with `schedules.task()` and managed by deployments, not by the imperative schedule methods.
