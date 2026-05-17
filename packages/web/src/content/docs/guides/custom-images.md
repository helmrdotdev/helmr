---
title: Custom images
description: Build task images with the tools your workflow needs.
section: Guides
sidebarLabel: Custom images
order: 350
---

# Custom images

Declare an image in TypeScript and attach it to a sandbox:

```ts
import { image, sandbox, task } from "@helmr/sdk"

const base = image("cli-tooling")
  .from("debian:trixie-slim")
  .run([
    "sh",
    "-ceu",
    "apt-get update && apt-get install -y --no-install-recommends ripgrep && rm -rf /var/lib/apt/lists/*",
  ])

const sbx = sandbox("cli-tooling")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })
```

Image builders support:

- `from(ref)` for the base image.
- `run(argv, opts)` for build commands.
- `copy(dest, source.file(...))` and `copy(dest, source.directory(...))` for task project files.
- `copyFrom(dest, image, srcPath)` for multi-image builds.
- `workdir(path)`, `env(key, value)`, and `user(name)`.

Most images do not need Bun installed. Helmr injects its TypeScript runtime before running task code, so install only the OS tools and application dependencies your task needs.

Tasks start in the checked-out workspace. Use relative paths for workspace files unless you intentionally need an image path such as `/opt/app/package.json`.
