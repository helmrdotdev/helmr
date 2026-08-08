---
title: Inspect a run
description: Read Run status, logs, events, and terminal results.
---

# Inspect a run

Start with the Run snapshot:

```sh
helmr run get RUN_ID --project agents --env development
helmr run wait RUN_ID --project agents --env development --timeout 10m
```

The snapshot includes status, entrypoint, Deployment, Workspace, current
attempt, cause, metadata, tags, timestamps, and terminal output or failure.

Use logs for process output and structured application logging:

```sh
helmr run logs RUN_ID --project agents --env development
helmr run logs RUN_ID --project agents --env development --follow
```

Use events for lifecycle decisions such as waits, retries, cancellation, and
finalization:

```sh
helmr run events RUN_ID --project agents --env development
```

The SDK exposes cursor pages and filters:

```ts
const run = await client.runs.retrieve("RUN_ID")
const logs = await client.runs.logs(run.id, {
  level: ["warn", "error"],
})
const events = await client.runs.events(run.id, {
  severity: "error",
})
```

Actor application output is not a Run log. Read it from the associated Session
when `run.sessionId` is present.
