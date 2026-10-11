---
title: Define an Agent
description: Select a Computer and handle serial inputs with optional setup.
---

# Define an Agent

Export an Agent and its Computer recipe from a configured declaration directory:

```ts
import { agent, computer, image } from "@helmr/sdk"

export const workspace = computer({
  id: "reviewer",
  image: image("reviewer").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const reviewer = agent({
  id: "reviewer",
  computer: workspace,
  async setup(ctx) {
    return { sessionId: ctx.session.id }
  },
  async turn(turn, ctx) {
    await turn.output.write("Review started")
    await turn.respond("Review complete")
    return { input: turn.input, sessionId: ctx.setupResult.sessionId }
  },
})
```

Helmr calls `turn` once per admitted input. Returning starts finalization; a
successful outcome requires drainage and the adequate durable disk Save. Throwing
fails that Turn after safe convergence. Place native completion validation and
repository checks before return. Stage a human response separately from the JSON
machine result.

Setup is optional and its result is reused across healthy continuation. Use
`turn.signal` for Turn work and `ctx.signal` for process/setup cancellation. For
native harnesses, open one managed harness in setup and preserve its conversation
identity across Turns. Follow the native SDK's permission and cancellation model.

Deploy, then [start the Agent](/docs/guides/how-to/start-an-agent). No manual
receive loop or completion call is required.
