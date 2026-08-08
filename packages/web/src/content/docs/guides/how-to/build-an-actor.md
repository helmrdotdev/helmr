---
title: Build an actor
description: Define a stable Actor with durable Session input and output.
---

# Build an actor

Use `actor()` for work that accepts follow-up messages or publishes progressive
application output:

```ts
import { actor } from "@helmr/sdk"

export const reviewer = actor({
  id: "reviewer",
  idleTimeout: "30m",
  async run(session, ctx) {
    const message = await session.input.receive({
      idleTimeout: "30m",
    })
    if (!message.ok) return
    await session.output.append(
      {
        type: "received",
        input: message.value,
        runId: ctx.run.id,
      },
      { idempotencyKey: `received:${message.record.id}` },
    )
  },
})
```

The handler receives an `ActorSession` and `ActorContext`. Session input is an
ordered JSON log; output is a separately ordered log. `idleTimeout` controls
how long the Actor can remain without progress, while `maxDuration`, `ttl`,
`retry`, and `queue` use the same Run-default contracts as Tasks.

Start the deployed Actor with a Workspace and optional stable key and initial
input:

```sh
helmr actor start reviewer \
  --project agents --env development \
  --workspace WORKSPACE_ID \
  --key github:helmrdotdev/helmr:42 \
  --input-json '{"type":"review","number":42}' \
  --idempotency-key github:helmrdotdev/helmr:42:start --json
```

The returned Session ID is the durable interaction address. The returned Run
ID identifies only the current boot execution.
