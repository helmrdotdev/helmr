---
title: TypeScript triggers
description: Start and observe Helmr runs from TypeScript.
section: Guides
sidebarLabel: TypeScript triggers
order: 370
---

# TypeScript triggers

Use the SDK runtime client from external TypeScript code when another service should start Helmr runs. The task id must already exist in a deployed task source.

```ts
import { HelmrClient, workspace } from "@helmr/sdk"
import { reviewPullRequest } from "./tasks/review-pull-request"

const client = new HelmrClient({
  url: process.env.HELMR_URL,
  apiKey: process.env.HELMR_API_KEY,
})

const handle = await client.tasks.trigger(reviewPullRequest, {
  payload: { prNumber: 42 },
  workspace: workspace.github("OWNER/REPO", {
    ref: "main",
    subpath: "path/to/task-project",
  }),
  secrets: {
    GITHUB_TOKEN: "vault:github-token",
  },
})
```

`tasks.trigger()` creates a run for the task id; it does not upload source. It returns a run handle. Retrieve, wait, inspect logs, or stream events from that handle:

```ts
const current = await client.runs.retrieve(handle)

if (current.pendingWaitpoint?.kind === "approval") {
  await client.waitpoints.approve(current.pendingWaitpoint, { reason: "reviewed" })
}

const finished = await client.runs.wait(handle, {
  timeoutMs: 10 * 60_000,
  intervalMs: 1_000,
})

const logs = await client.runs.logs.retrieve(handle)
const events = await client.runs.events.list(handle)
```

The client also reads `HELMR_URL` and `HELMR_API_KEY` from the environment when options are omitted. HTTPS control URLs require an API key. Plain HTTP is accepted only for loopback hosts.

Payload is persisted as audit data. Keep credentials out of payload and pass declared task secrets as vault references.
