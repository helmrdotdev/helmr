---
title: Actors
description: Stable interactive workflows backed by Sessions and managed Runs.
---

# Actors

An Actor is a deployed definition for continuing, stateful interaction. It has
a stable Session and fixed ordered input and output logs. Execution is still
performed by Runs, so a Session can span a boot Run and later continuation Runs.

```ts
import { actor } from "@helmr/sdk"

export const assistant = actor({
  id: "assistant",
  idleTimeout: "30m",
  async run(session, ctx) {
    const received = await session.input.receive({
      idleTimeout: "30m",
    })
    if (!received.ok) return
    await session.output.append({
      type: "acknowledged",
      inputSequence: received.record.sequence,
      runId: ctx.run.id,
    })
  },
})
```

Actor start requires a Workspace and may include a stable key, initial input,
an idempotency key, and managed Run options. It returns a Session reference and
the initial Run handle. The optional initial input is the first ordinary input
record, not a separate handler argument.

Only the Actor handler consumes its input cursor. External callers append
through the Session input API and independently page through output. Each input
record has a sequence, source, and timestamp. Each output record also carries
Run attempt and Deployment provenance.

Sessions have `open`, `closed`, `cancelled`, and `failed` states. Closing a
Session prevents further interaction. An idle timeout or managed Run failure
can also end progress, so Actor protocols should handle receive errors and use
stable idempotency keys for upstream messages and derived output.

Use a Task for one terminal request and result. Use a Token for one externally
completed value. Use an Actor when the outside system needs a continuing
channel or the application needs progressive durable output.
