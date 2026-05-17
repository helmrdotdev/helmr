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

const img = image("agent")
  .from("debian:trixie-slim")
  .run(["sh", "-ceu", "apt-get update && apt-get install -y git ripgrep"])
  .copy("/opt/task", source.directory("./tasks"))

export const sb = sandbox("agent")
  .image(img)
  .workspace("/workspace")
  .resources({ cpu: 2, memory: "4Gi" })
```

## Images

Images are built from ordered steps: `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, and `user`. Build steps can use cache mounts and build-time secret mounts.

Task images do not need to install Bun just to run TypeScript task code. Helmr injects its runtime adapter into the guest before executing the task.

## Workspace Mount

The default workspace mount is `/workspace`. You can set another absolute mount path with `sandbox.workspace("/path")`, except reserved runtime paths such as `/dev`, `/proc`, `/sys`, `/run`, `/tmp`, and `/opt/helmr`.

## Resources

Use `resources({ cpu, memory })` to request vCPU count and memory for the sandbox. Worker capabilities determine whether a run can be claimed.
