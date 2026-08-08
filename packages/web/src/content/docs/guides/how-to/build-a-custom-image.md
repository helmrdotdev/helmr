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
    "/usr/local/bin/review-helper",
    source.file("bin/review-helper"),
  )
  .run(["chmod", "+x", "/usr/local/bin/review-helper"])
  .workdir("/workspace")

export const reviewerSandbox = sandbox({ id: "reviewer" })
  .image(runtime)
  .resources({ cpu: 2, memory: "2GiB" })
```

The builder supports `from`, `run(argv)`, `copy`, `copyFrom`, `workdir`, `env`,
and `user`. Use `source.file()` and `source.directory()` for files that must be
baked into the Workspace image. Commands are argv arrays; invoke a shell
explicitly when you need shell syntax.

For a private base image, pass `from(ref, { auth: { username, password:
secrets.fromName("REGISTRY_PASSWORD") } })`. The password is a Secret address,
not a literal value.

Helmr mounts the selected Node runtime as a managed runtime artifact. A separate
immutable Program artifact supplies the compiled declaration modules and
installed project dependency tree. The Workspace image supplies the Linux base,
OS libraries, and tools your work invokes; do not reinstall the managed runtime
or project dependencies into it. The Sandbox separately declares CPU and memory.
