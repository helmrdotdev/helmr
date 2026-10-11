---
title: Build a durable Agent
description: Retain setup state while processing continuing inputs in one Session.
---

# Build a durable Agent

An Agent Session processes serial Turns and retains setup state through healthy
continuation. This example counts Turns in a live setup object. It demonstrates
Session state without requiring a model provider.

## Define the Agent

```ts
import { agent, computer, image } from "@helmr/sdk"

export const workspace = computer({
  id: "assistant",
  image: image("assistant").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const assistant = agent({
  id: "assistant",
  computer: workspace,
  async setup(ctx) {
    return { turns: 0, recovery: ctx.recovery.kind }
  },
  async turn(turn, ctx) {
    ctx.setupResult.turns += 1
    await turn.output.write("Processing input")
    await turn.respond(`Processed Turn ${ctx.setupResult.turns}`)
    return { input: turn.input, turns: ctx.setupResult.turns }
  },
})
```

Healthy hibernation preserves the live object and native processes. The counter
is not a separate durable database: exceptional reconstruction reruns setup and
resets it. Applications that reconstruct important state should use retained disk
or their own authoritative store and reconcile external effects.

For a model-backed Agent, initialize its managed native harness in setup and use
that same harness in each Turn. Read the text parts from `turn.input` explicitly,
propagate `turn.signal`, validate native completion and finish post-processing
before return. Select human response content separately from raw native events.

## Deploy and start

```sh
helmr deploy . --project demo --env development
helmr agent start assistant --project demo --env development \
  --input-json '[{"type":"text","text":"first"}]' --session-key user:ada \
  --idempotency-key tutorial:assistant:1 --json
```

The receipt returns a Session and first Turn. Save the Session ID for later
interaction. A Session key chooses the conversation; an idempotency key reconciles
one input request.

## Continue the Session

```sh
helmr session enqueue SESSION_ID --project demo --env development \
  --input-json '[{"type":"text","text":"second"}]' --idempotency-key tutorial:assistant:2 --json
helmr session events SESSION_ID --project demo --env development --after 0 --json
```

Read event pages using `next_after` for the next `--after` value. Observe each Turn
with `helmr session turn get` or bounded `turn wait`; output EOF is not completion.
Promotion does not upgrade the existing Session's Agent code.

## Interrupt or finish

```sh
helmr session interrupt SESSION_ID --project demo --env development --json
helmr session get SESSION_ID --project demo --env development --json
helmr session resume SESSION_ID --hold HOLD_ID --project demo --env development
helmr session close SESSION_ID --project demo --env development
```

Use the exact hold ID returned by interrupt, and its owning Session for resume.
Interrupt preserves queued Turns; resume permits future work without replaying the
interrupted input. Close drains accepted work but does not clear holds. Receipts
acknowledge controls; inspect retained state for convergence.
