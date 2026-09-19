---
title: Actors
description: Stable Sessions, explicit Turns and managed execution Runs.
---

# Actors

An Actor is a deployed definition for continuing interaction. A Session is its
stable identity. Turns are FIFO units of work within the Session, and Runs provide
the execution that serves them. One Run can process multiple Turns.

```ts
import { actor } from "@helmr/sdk"

export const assistant = actor({
  id: "assistant",
  idleTimeout: "90s",
  async run(session, ctx) {
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      await turn.output.write({ type: "acknowledged", turnId: turn.id, runId: ctx.run.id })
      await turn.complete()
    }
  },
})
```

Start with a Workspace and optional stable key, idempotency key and Run options.
Then enqueue the first Turn separately. `session.receive()` consumes FIFO work;
`turn.onMessage` handles interactions within that work. Interface routing, provider
SDKs, native questions and approvals remain editable application code.

Input, output and lifecycle events share a retained sequence. An output stream
ending does not complete a Turn: finish post-processing and explicitly call
`turn.complete` or `turn.fail`. A failed Turn and a failed Session are distinct.

Sessions are `open`, `closing`, `closed`, or `failed`. Closing drains accepted
work. Interruption retains later Turns behind a hold until explicit resume; it
does not replay the interrupted Turn. Uncertain execution requires reconciliation.
An idle timeout can shorten the warm wait before checkpoint/suspension without
closing the Session. Arbitrary provider sockets and promises are not managed waits.

Use a Task for bounded work, a Token for one externally completed value, and an
Actor for continuing interaction with explicit work and message lifetimes. See
[Actors, Sessions and Turns](/docs/reference/sdk/actors-and-sessions) for the API.
