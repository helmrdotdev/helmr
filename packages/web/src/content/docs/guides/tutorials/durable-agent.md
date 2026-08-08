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
  idleTimeout: "30m",
  async run(session, ctx) {
    await session.output.append({
      type: "ready",
      runId: ctx.run.id,
      workspaceId: ctx.workspace.id,
    })

    while (!ctx.signal.aborted) {
      const message = await session.input.receive({
        idleTimeout: "30m",
      })
      if (!message.ok) return
      await session.output.append({
        type: "reply",
        inputSequence: message.record.sequence,
        text: `received: ${JSON.stringify(message.value)}`,
      })
    }
  },
})
```

`receive()` can durably park the managed Run. New input resumes work without
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
  --input-json '{"type":"message","text":"hello"}' \
  --idempotency-key tutorial:assistant:start \
  --json
```

The response contains both `session_id` and the boot `run_id`. Save the Session
ID for all later interaction.

## Continue the Session

```sh
helmr actor input send SESSION_ID \
  --project demo --env development \
  --input-json '{"type":"message","text":"summarize our work"}' \
  --idempotency-key tutorial:assistant:message:2

helmr actor output read SESSION_ID \
  --project demo --env development \
  --after 0 --jsonl
```

Output reads are finite pages. Pass the last durable sequence back through
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
