---
title: Start a task
description: Start a deployed Task in an existing Workspace.
---

# Start a task

Every external Task start names an existing Workspace:

```sh
helmr task start review-pr \
  --project agents --env development \
  --workspace WORKSPACE_ID \
  --payload-json '{"owner":"helmrdotdev","repo":"helmr","number":42}' \
  --idempotency-key github:helmrdotdev/helmr:pr:42 \
  --wait
```

Use `--payload-file payload.json` for a JSON document, or repeat
`--payload KEY=VALUE` for top-level string fields. Tasks without a declared
payload schema do not accept payload.

`--wait` waits for the terminal result. `--follow` streams logs until the Run
finishes. For asynchronous starts, omit both and save the returned Run ID.

The SDK preserves Task input and output types:

```ts
const workspace = client.workspaces.ref("WORKSPACE_ID")
const run = await client.tasks.start<typeof reviewPr>(
  reviewPr.id,
  {
    payload: {
      owner: "helmrdotdev",
      repo: "helmr",
      number: 42,
    },
    workspace,
    idempotencyKey: "github:helmrdotdev/helmr:pr:42",
  },
)
const output = await client.runs.wait(run).unwrap()
```

Run options may set a queue, concurrency key, priority, queued TTL, retry
policy, metadata, and tags. They do not override the Task's maximum execution
duration. Never pass secret values in payload or metadata.
