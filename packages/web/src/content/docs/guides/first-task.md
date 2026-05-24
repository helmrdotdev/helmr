---
title: First task
description: Create the smallest Helmr task project and understand the task shape.
section: Guides
sidebarLabel: First task
order: 300
---

# First task

A Helmr task project has a `helmr.config.ts` file and one or more exported task modules.

```sh
helmr init --dir ./my-helmr-tasks
```

`helmr init` creates:

- `helmr.config.ts`, which tells Helmr where to find tasks.
- `package.json`, which declares the Helmr SDK dependency.
- `tasks/hello.ts`, a starter task.

The starter shape is:

```ts
import { cache, image, sandbox, source, task } from "@helmr/sdk"

const runtime = image("hello")
  .from("node:24-bookworm-slim")
  .workdir("/app")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/app/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("hello-bun") }],
  })

const sb = sandbox("hello")
  .image(runtime)
  .workspace("/app")

export const hello = task({
  id: "hello",
  sandbox: sb,
  run: async () => ({ ok: true }),
})
```

Tasks declare their runtime before they run: image, sandbox, workspace mount, resources, secrets, max duration, payload type, and return value.

Use `ctx` for runtime interaction:

```ts
import { writeFile } from "node:fs/promises"

export const hello = task({
  id: "hello",
  sandbox: sb,
  maxDuration: 300,
  run: async (payload: { name?: string }, ctx) => {
    const greeting = `hello ${payload.name?.trim() || "Helmr"}`
    await writeFile("hello.txt", `${greeting}\nrun=${ctx.run.id}\n`)
    ctx.log.info({ message: "wrote greeting", path: "hello.txt" })
    return { greeting, runId: ctx.run.id }
  },
})
```

Keep payload for audit-safe inputs such as PR numbers, repository names, ticket ids, and flags. Do not put tokens or credentials in payload; declare secrets and bind them at run time.
