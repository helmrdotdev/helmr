---
title: Agents
description: Define Computer placement, setup and one handler for each admitted input.
---

# Agents

An Agent is a deployed definition. It selects a Computer recipe and handles one
Turn at a time in each Session. The same model supports a single request or a
continuing conversation.

```ts
import { agent, computer, image } from "@helmr/sdk"

const workspace = computer({
  id: "workspace",
  image: image("workspace").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const reviewer = agent({
  id: "reviewer",
  computer: workspace,
  async setup(ctx) {
    return { sessionId: ctx.session.id, turns: 0 }
  },
  async turn(turn, ctx) {
    ctx.setupResult.turns += 1
    await turn.output.write("Review started")
    await turn.respond("Review complete")
    return { input: turn.input, turns: ctx.setupResult.turns }
  },
})
```

`setup` is optional. Its result may contain mutable objects or native handles.
Healthy hibernation preserves it; exceptional reconstruction can rerun setup
against retained disk. It is not a separate durable key/value store. Use
`ctx.recovery` to distinguish initialization from reconstruction and reconcile
external effects in application code.

`turn` is required. Inputs are JSON arrays of text parts; machine results are arbitrary JSON.
Validate application-specific commands in the handler. Await output,
questions and native work before returning. Throwing fails the Turn; it does not
request an automatic retry.

`maxTurnDuration` optionally limits elapsed time through processing, human waits
and finalization. `closeAfterIdle` optionally closes the Session identity; it is
separate from compute release. Neither has a universal duration default.

Use `turn.signal` to cancel Turn work and `ctx.signal` for setup/process shutdown.
The managed MCP connection carries Session runtime authority. Root Sessions can
create owned children with `spawn` or independent roots with `start`. An explicit
`HelmrClient` uses only its supplied authentication, including inside Agent code.

See [Agents, Sessions and Turns](/docs/reference/sdk/agents-and-sessions) for
client operations and [Schedules](/docs/concepts/schedules) for cron triggers.
