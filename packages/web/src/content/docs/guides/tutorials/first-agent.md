---
title: Run your first Agent
description: Deploy a small Agent and observe its first completed Turn.
---

# Run your first Agent

This tutorial adds business-input validation and a file write to the
[Quickstart](/docs/quickstart). You need an existing Project Environment and
active worker capacity.

## Create the project

```sh
helmr init --dir ./hello-helmr
cd ./hello-helmr
bun install
```

The generated config discovers exported declarations in `tasks/`. This directory
name is configuration, not an execution primitive. Replace `tasks/hello.ts` with:

```ts
import { agent, computer, image } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

export const workspace = computer({
  id: "hello",
  image: image("hello").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const hello = agent({
  id: "hello",
  computer: workspace,
  maxTurnDuration: "5m",
  async turn(turn) {
    const name = turn.input.map(part => part.text).join("").trim()
    if (!name) throw new Error("Input requires a name")
    const greeting = `Hello ${name}`
    await turn.output.write("Writing your greeting")
    await writeFile("/workspace/greeting.txt", `${greeting}\n`, { flush: true })
    await turn.respond(greeting)
    return { greeting, path: "/workspace/greeting.txt" }
  },
})
```

The handler reads and validates the input text, finishes its write, stages a human greeting
and returns a separate machine result. Completed requires the adequate Computer
Save. The path in the result is not an immutable file artifact or download link.

## Deploy and start

```sh
helmr login
helmr deploy . --project demo --env development
helmr agent start hello --project demo --env development \
  --input-json '[{"type":"text","text":"Ada"}]' \
  --idempotency-key tutorial:hello:1 --json
```

The start receipt contains `session_id` and `turn_id`. A Computer is created from
the Agent's recipe. Store both IDs; retry an uncertain request with the same key
and exact input.

## Read progress and the outcome

```sh
helmr session events SESSION_ID --project demo --env development --after 0 --json
helmr session turn wait SESSION_ID TURN_ID --project demo --env development --timeout 10m --json
helmr session turn get SESSION_ID TURN_ID --project demo --env development --json
```

Events are readable during processing and finalization. A wait timeout leaves the
Turn running; use the same IDs to observe it again. A settled `completed` outcome
contains the result and committed greeting response. Failure or interruption must
be handled separately.

## Continue and close

```sh
helmr session enqueue SESSION_ID --project demo --env development \
  --input-json '[{"type":"text","text":"Grace"}]' --idempotency-key tutorial:hello:2
helmr session close SESSION_ID --project demo --env development
```

The second Turn uses the same Computer and overwrites the greeting file. Close
drains accepted work and rejects ordinary new admission. The Computer remains
until explicitly deleted after its associated work and cleanup permit deletion.
Continue with [Build a durable Agent](/docs/guides/tutorials/durable-agent).
