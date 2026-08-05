---
title: Runtime client
description: TypeScript client APIs for Tasks, Runs, Actors, Workspaces, Tokens, and deployment resources.
section: Reference
sidebarLabel: Runtime client
order: 920
---

# Runtime client

`HelmrClient` is the explicit authenticated TypeScript client:

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL!,
  apiKey: process.env.HELMR_API_KEY!,
})
```

Declaration-backed resources use a declared ID at runtime and `typeof
definition` for compile-time inference. Importing a definition is type-only and
does not bundle its handler into the calling backend:

```ts
import type { resizeImage } from "./helmr-definitions"

const workspace = await client.sandboxes.createWorkspace(
  "repository-agent",
  {
    key: "repo:helmrdotdev/helmr",
    idempotencyKey: "workspace:helmrdotdev/helmr",
  },
)

const run = await client.tasks.start<typeof resizeImage>(
  "resize-image",
  {
    payload: { imageId: "img_123" },
    workspace,
    idempotencyKey: "resize:img_123",
    metadata: { customerId: "cus_123" },
    tags: ["image"],
  },
)

const result = await client.runs.wait<typeof resizeImage>(run.id)
```

The request shape is consistent across the client:

- a target ID is the first argument;
- domain input is one flat request object;
- `AbortSignal` is in the final transport `RequestOptions`;
- list filters and pagination use a query object.

Main surfaces:

| API | Purpose |
| --- | --- |
| `client.tasks.start<typeof task>(declaredId, request, options?)` | Start a Task Run in an existing Workspace. |
| `client.runs.retrieve(runId, options?)` | Retrieve a Run snapshot. |
| `client.runs.list(query?, options?)` | List Run snapshots. |
| `client.runs.wait<typeof task>(runId, options?)` | Wait for a terminal Run result. |
| `client.runs.logs(runId, query?, options?)` | Read one finite page of Run logs. |
| `client.runs.events(runId, query?, options?)` | Read one finite page of Run events. |
| `client.actors.start<typeof actor>(declaredId, request, options?)` | Start or address an Actor. |
| `client.sessions.ref(sessionId)` | Create a typed Session reference by UUID. |
| `client.sandboxes.createWorkspace(declaredId, request?, options?)` | Create a Workspace from a deployed Sandbox declaration. |
| `client.workspaces.ref(workspaceId)` | Create a typed Workspace reference by UUID. |
| `workspace.retrieve(options?)` | Retrieve Workspace state. |
| `workspace.files.read/stat/list(...)` | Read the current committed filesystem. |
| `workspace.exec(request, options?)` | Execute one bounded command and return its terminal output. |
| `workspace.delete(request?, options?)` | Delete the Workspace. |
| `client.tokens.create(request?, options?)` | Create an externally completable Token. |
| `client.tokens.retrieve/list/complete/cancel(...)` | Inspect or complete Token state. |
| `client.secrets.create/retrieve/list/rotate/revoke(...)` | Manage versioned Secret values. |
| `client.deployments.retrieve/current(...)` | Inspect Deployments. |
| `client.schedules.retrieve/list(...)` | Inspect source-declared Schedules. |

Task start always names an existing Workspace. Workspace capacity is selected by
the deployed declaration; disk size is an internal runtime fact, not a required
public create input.

Run log and event calls return finite cursor pages. Poll using `nextCursor` when
following progress. A Run snapshot is the source of truth for terminal status.
Run records expose canonical resource UUIDv7 values; trace authority is not
part of this client contract.

Each Actor has exactly two fixed durable channels, `input` and `output`. They
are not named or separately declared. Use tagged JSON unions for
application-level message kinds. Use a Token for one externally completed
approval value.

Schedules are declared only in source and reconciled by Deployment promotion.
External Schedule APIs are read-only.
