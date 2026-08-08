---
title: Tasks
description: Define, start, call, and type TypeScript Tasks.
---

# Tasks

`task(config)` defines a one-shot entrypoint. With `payload`, its Standard
Schema input type is inferred; without `payload`, the handler receives only the
context.

```ts
export const resize = task({
  id: "resize",
  payload: resizePayload,
  maxDuration: "10m",
  run: async (input, ctx) => ({
    id: input.id,
    runId: ctx.run.id,
  }),
})
```

Definition defaults are `queue`, `maxDuration`, `ttl`, and `retry`. Runtime
options add `concurrencyKey`, `priority`, `metadata`, and `tags`. Every start
requires a `WorkspaceRef`.

`definition.start(payload?, { workspace, idempotencyKey?, ... })` returns a
typed `RunHandle`. `definition.call(payload?, { workspace, idempotencyKey,
... })` waits and returns a `TaskWait`; `await wait` yields `{ ok, ... }` and
`await wait.unwrap()` returns output or throws the Run failure.

External code uses `client.tasks.retrieve`, `list`, and
`start<typeof definition>(declaredId, request)`. `TaskInput<T>` and
`TaskOutput<T>` expose inferred types.

`queue({ name, concurrencyLimit? })` creates a reusable queue declaration.
Queue names allow letters, digits, `.`, `_`, `/`, and `-`, up to 256 characters.
