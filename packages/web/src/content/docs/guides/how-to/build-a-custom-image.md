---
title: Build a custom image
description: Declare the runtime image and resources for a Sandbox.
---

# Build a custom image

Compose an image in TypeScript and attach it to a Sandbox:

```ts
import { image, sandbox, source } from "@helmr/sdk"

const runtime = image("reviewer")
  .from("debian:bookworm-slim")
  .run([
    "sh",
    "-ceu",
    "apt-get update && apt-get install -y git ripgrep",
  ])
  .copy(
    source.file("bin/review-helper"),
    "/usr/local/bin/review-helper",
  )
  .run(["chmod", "+x", "/usr/local/bin/review-helper"])
  .workdir("/workspace")

export const reviewerSandbox = sandbox({ id: "reviewer" })
  .image(runtime)
  .resources({ cpu: 2, memory: "2GiB" })
```

The builder supports `from`, `run(argv)`, `copy`, `copyFrom`, `workdir`, `env`,
and `user`. Use `source.file()` and `source.directory()` for files that must be
baked into the Workspace image. `copy(source, destination)` names the project
source first and the absolute image path second, as Dockerfile `COPY` does. A
file is written at the destination path, or inside it when that path is already
a directory in the image; a directory's contents are merged into the
destination. Commands are argv arrays; invoke a shell
explicitly when you need shell syntax.

A Workspace image is where Program code and the tools it spawns run. The Linux
environment that *installs* your dependencies is a different role, prepared with
[`build.builder`](/docs/reference/configuration#build-settings); packages added
there are not part of any Workspace image.

Helmr mounts the selected Node runtime as a managed runtime artifact. A separate
immutable Program artifact supplies the compiled declaration modules and
installed project dependency tree. The Workspace image supplies the Linux base,
OS libraries, and tools your work invokes; do not reinstall the managed runtime
or project dependencies into it. The Sandbox separately declares CPU and memory.
