---
title: Custom images
description: Build task images with the tools your workflow needs.
section: Guides
sidebarLabel: Custom images
order: 350
---

# Custom images

Declare an image in TypeScript and attach it to a Sandbox definition:

```ts
import { image, sandbox, source } from "@helmr/sdk"

const base = image("cli-tooling")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"])
  .run([
    "sh",
    "-ceu",
    "apt-get update && apt-get install -y --no-install-recommends ripgrep && rm -rf /var/lib/apt/lists/*",
  ])

export const cliToolingSandbox = sandbox({ id: "cli-tooling" })
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })
```

Image builders support:

- `from(ref)` for the base image.
- `run(argv)` for build commands.
- `copy(dest, source.file(...))` and `copy(dest, source.directory(...))` for task project files.
- `copyFrom(dest, image, srcPath)` for multi-image builds.
- `workdir(path)`, `env(key, value)`, and `user(name)`.

TypeScript task images must provide Node.js 22.18 or newer as `node` on `PATH`. Helmr injects the adapter protocol, but task code runs with the language runtime and dependencies installed in your image. Install the package manager, OS tools, and application dependencies your task needs as explicit image build steps.

Tasks and direct workspace operations start in the mounted workspace directory
unless a different working directory is supplied. Use relative paths for
workspace files unless you intentionally need an image path such as
`/opt/app/package.json`.
