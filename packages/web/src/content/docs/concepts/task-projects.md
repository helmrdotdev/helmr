---
title: Task projects
description: The source layout Helmr indexes and deploys.
section: Concepts
sidebarLabel: Task projects
order: 120
---

# Task Projects

A task project is the directory you deploy with `helmr deploy`. It is a package-managed TypeScript project with `package.json`, `helmr.config.ts`, and one or more task modules.

`package.json` must declare `@helmr/sdk` in `dependencies` and an explicit `packageManager`. `helmr init` creates this structure for new task projects.

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  project: "agents",
  dirs: ["./tasks"],
})
```

`dirs` is required and must contain at least one directory inside the project root. Helmr discovers `.ts`, `.mts`, `.cts`, `.js`, `.mjs`, and `.cjs` files in those directories and indexes exported `task(...)` definitions.

Task modules may also define deployment-level stream catalog entries with the module-level `streams.input(...)` and `streams.output(...)` primitives. Optional stream schemas validate runtime reads and writes; the catalog entry itself is the stream name and direction. `task(...)` is only the execution definition; do not redeclare streams inside task config.

## Deployment

`helmr deploy PATH` validates `package.json`, installs missing task project dependencies locally with the declared `packageManager` for config inspection, indexes task IDs, module paths, and export names, creates a deployment-source archive, uploads it with its content hash, and activates the deployment for the selected project environment.

Remote deployment builds install archived task project dependencies in a product-managed build environment using the explicit `packageManager` from `package.json`. Task execution uses dependencies installed in the task sandbox image. Install runtime dependencies during the image build so imports resolve from the sandbox, not from the deployment archive.

Default deployment excludes tests, files prefixed with `_`, `node_modules`, `.git`, `.helmr`, `.next`, `.env`, and `.env.*`. Set `ignorePatterns` in `helmr.config.ts` to control project-specific excludes.

## Project Hint

`defineConfig` requires a `project` string. Deploy uses that source-controlled project as the target instead of accepting a CLI project override.
