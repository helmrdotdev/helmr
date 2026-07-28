---
title: Task projects
description: The source layout Helmr indexes and deploys.
section: Concepts
sidebarLabel: Task projects
order: 120
---

# Task Projects

A task project is the directory you deploy with `helmr deploy`. It contains
package-managed JavaScript or TypeScript source, an exact lockfile,
`helmr.config.ts`, and one or more declaration modules. An optional
`.helmrignore` explicitly removes paths from submitted source.

`package.json` selects exact Node and Manager releases. Helmr compiles project
and workspace-local source with its pinned esbuild toolchain. Registry
dependencies remain external and are loaded from the Manager-produced
`node_modules` tree. The Managed Runtime executes only generated JavaScript.

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["tasks"],
})
```

`dirs` is required and must contain at least one directory inside the
submitted project tree. Helmr recursively discovers supported JavaScript and
TypeScript modules in deterministic path order.

Task projects may export Tasks, Actors, Workspaces, and source-declared
Schedules. Named Run or Session stream declarations are not part of v0;
durable interactive I/O belongs to an Actor's fixed input/output channels.

## Deployment

`helmr deploy PATH` validates source controls, creates a deployment-source
archive, uploads it with its content hash, and follows the remote build. It
does not install dependencies, execute config, read `node_modules`, or mutate
the local project or lockfile.

The remote build fetches dependencies without lifecycle scripts, removes
network authority, performs the frozen install and ordinary lifecycle scripts,
evaluates config once for that build attempt, compiles and discovers
declarations, and publishes the complete installed project tree with generated
modules as one immutable Program.

`.helmrignore` alone selects submitted source. Discovery-specific
`ignorePatterns` never remove source or runtime bytes. Login/session deploys
require explicit `--project` and `--env`; environment API keys already carry
that scope and reject those flags.
