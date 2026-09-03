---
title: Run your first task
description: Create, deploy, and run a small Helmr task from a durable workspace.
sidebarLabel: First task
---

# Run your first task

This tutorial takes a new TypeScript project from source to a completed Run.

## Create the project

```sh
helmr init --dir ./hello-helmr
cd ./hello-helmr
bun install
bun add zod
```

The generated project includes `helmr.config.ts`, a `tasks` directory,
`.helmrignore`, `package.json`, and TypeScript configuration. `bun install`
creates a frozen lockfile for reproducible builds; this tutorial also adds Zod
for payload validation. The generated config selects the declaration directories
that the local Helmr builder discovers:

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({ dirs: ["tasks"] })
```

A runnable project exports both a Sandbox and a Task. The Sandbox defines the
image and resources; the Task defines the input contract and work:

```ts
import { image, sandbox, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"
import { z } from "zod"

const runtime = image("hello")
  .from("debian:bookworm-slim")
  .workdir("/workspace")

export const helloSandbox = sandbox({ id: "hello" })
  .image(runtime)
  .resources({ cpu: 1, memory: "1GiB" })

export const hello = task({
  id: "hello",
  payload: z.object({ name: z.string() }),
  maxDuration: "5m",
  async run({ name }, ctx) {
    const greeting = `hello ${name}`
    await writeFile("greeting.txt", `${greeting}\n`)
    return { greeting, runId: ctx.run.id }
  },
})
```

Payload schemas use the Standard Schema v1 contract; Zod 4 is supported. Keep
credentials out of payloads because payload is stored as Run data.

## Deploy and create a workspace

Log in, then deploy into an existing project and environment:

```sh
helmr login
helmr deploy . --project demo --env development
```

The CLI builds an immutable bundle containing the task and Sandbox declarations,
then promotes the verified Deployment by default. Create a durable Workspace from the promoted
Sandbox:

```sh
WORKSPACE_ID="$(helmr workspace create hello \
  --project demo --env development \
  --key tutorial:first-task \
  --idempotency-key tutorial:first-task:workspace)"
```

The key is an optional stable lookup value. The idempotency key makes a retried
create request safe.

## Start and inspect the Run

```sh
helmr task start hello \
  --project demo --env development \
  --workspace "$WORKSPACE_ID" \
  --payload-json '{"name":"Ada"}' \
  --idempotency-key tutorial:first-task:run \
  --wait
```

The start returns a Run ID. `--wait` also waits for the terminal result. The
Workspace remains after the Run. You can use Workspace exec to print the file
the Task committed:

```sh
helmr workspace exec \
  --project demo --env development --id "$WORKSPACE_ID" \
  --idempotency-key tutorial:read-greeting -- cat greeting.txt
```

This requires `workspace-exec:create`. Exec runs a command and may mutate the
Workspace; it is not a read-only file inspection API.

Next, see [Inspect a run](/docs/guides/how-to/inspect-a-run/) for logs and
events, or [Durable agent](/docs/guides/tutorials/durable-agent/) for continuing
input and output.
