---
title: Build an actor
description: Receive explicit Turns and publish durable output.
---

# Build an actor

```ts
import { actor } from "@helmr/sdk"

export const reviewer = actor({
  id: "reviewer",
  idleTimeout: "90s",
  async run(session, ctx) {
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      await turn.output.write({ input: turn.input, runId: ctx.run.id }, {
        idempotencyKey: `received:${turn.id}`,
      })
      await turn.complete()
    }
  },
})
```

The custom `run` loop owns execution. A receive returns the next Turn or `null`
when closing has drained. Output is durable application data; explicit completion
marks the outcome. Put any tests or other post-processing before completion.

Start with a Workspace, then enqueue the first work using the returned Session ID:

```sh
helmr actor start reviewer --project agents --env development \
  --workspace WORKSPACE_ID --key review:42 \
  --idempotency-key review:42:start --json

helmr actor enqueue SESSION_ID --project agents --env development \
  --data-json '{"type":"review","number":42}' \
  --idempotency-key review:42:first --json
```

The Session ID addresses continuing interaction; the Run ID identifies execution.
Use the returned Turn ID for an exact follow-up or interruption. The idle timeout
controls managed suspension; receive `timeout` is a separate application deadline.
