---
title: Build a durable agent
description: Build an Actor that receives follow-up input and publishes durable output.
sidebarLabel: Durable agent
---

# Build a durable agent

Tasks are a good fit for bounded one-shot work. An Actor adds a stable Session
with ordered input and output, while each period of execution is still a Run.

## Define the Actor

Add a declaration beside your existing Sandbox:

```ts
import { actor } from "@helmr/sdk"

export const assistant = actor({
  id: "assistant",
  idleTimeout: "90s",
  async run(session, ctx) {
    await session.output.write({ type: "ready", runId: ctx.run.id })
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      await turn.output.write({ type: "reply", received: turn.input })
      await turn.complete()
    }
  },
})
```

The Actor-level `idleTimeout` supplies the default hot-wait duration for Session
input, Token, and timer waits inside the Actor. An individual wait can override
it where its API accepts `idleTimeout`. It is separate from a response timeout
and is not a billing cap. `receive()` can durably park the
managed Run without closing the Session. New input resumes work without
discarding the Session's identity or ordered history. A later continuation may
use a different Run ID.

## Deploy and start

```sh
helmr deploy . --project demo --env development

WORKSPACE_ID="$(helmr workspace create hello \
  --project demo --env development \
  --key tutorial:assistant \
  --idempotency-key tutorial:assistant:workspace)"

helmr actor start assistant \
  --project demo --env development \
  --workspace "$WORKSPACE_ID" \
  --key user:ada \
  --idempotency-key tutorial:assistant:start \
  --json
```

The response contains both `session_id` and the boot `run_id`. Save the Session
ID for all later interaction.

## Continue the Session

```sh
helmr actor enqueue SESSION_ID \
  --project demo --env development \
  --data-json '{"type":"message","text":"summarize our work"}' \
  --idempotency-key tutorial:assistant:message:2

helmr actor events SESSION_ID \
  --project demo --env development \
  --after 0 --jsonl
```

Event reads are finite pages; output and lifecycle events share one sequence.
A page ending does not complete a Turn. Pass the last durable sequence back through
`--after` to read only newer records. Input idempotency keys should come from a
stable upstream event ID so delivery retries do not duplicate application
commands.

Inspect the current managed Run with `helmr actor get SESSION_ID`. When the
conversation is finished, close the Session explicitly:

```sh
helmr actor close SESSION_ID \
  --project demo --env development \
  --idempotency-key tutorial:assistant:close
```

Closing stops future input admission; it does not turn Actor output into Run
logs. Use the Session output channel for application messages and Run logs and
events for execution diagnostics.
