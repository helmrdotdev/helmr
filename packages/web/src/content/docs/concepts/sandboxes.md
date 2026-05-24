---
title: Sandboxes
description: How tasks declare images, resources, and workspace mounts.
section: Concepts
sidebarLabel: Sandboxes
order: 140
---

# Sandboxes

A sandbox declares the Linux runtime shape for a task. Every task must reference a sandbox, and every sandbox must declare an image.

```ts
import { image, sandbox, source, cache } from "@helmr/sdk"

const deps = cache("bun-install")
const installTools = [
  "apt-get update",
  "apt-get install -y --no-install-recommends git ripgrep",
  "rm -rf /var/lib/apt/lists/*",
].join(" && ")

const img = image("agent")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .run(["sh", "-ceu", installTools])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: deps }],
  })
  .copy("/opt/task", source.directory("./tasks"))

export const sb = sandbox("agent")
  .image(img)
  .workspace("/workspace")
  .resources({ cpu: 2, memory: "4Gi" })
```

## Images

Images are built from ordered steps: `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, and `user`. Build steps can use cache mounts and build-time secret mounts.

TypeScript task images must provide Node.js 22.18 or newer as `node` on `PATH`. Helmr injects its adapter into the guest, but the task code runs with the Node runtime and dependencies installed in your image. Install any package manager, command-line tools, and task dependencies your code uses as explicit image build steps.

## Workspace Mount

The default workspace mount is `/workspace`. You can set another absolute mount path with `sandbox.workspace("/path")`, except reserved runtime paths such as `/dev`, `/proc`, `/sys`, `/run`, `/tmp`, and `/opt/helmr`.

## Resources

Use `resources({ cpu, memory })` to request vCPU count and memory for the sandbox. Worker capabilities determine whether a run can be started.
