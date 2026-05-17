---
title: Task projects
description: The source layout Helmr indexes and deploys.
section: Concepts
sidebarLabel: Task projects
order: 120
---

# Task Projects

A task project is the directory you deploy with `helmr deploy`. It contains `helmr.config.ts` and one or more task modules.

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["./tasks"],
})
```

`dirs` is required and must contain at least one directory inside the project root. Helmr discovers TypeScript and JavaScript files in those directories and indexes exported `task(...)` definitions.

## Deployment

`helmr deploy PATH` loads the config, indexes task IDs, module paths, and export names, creates a task-source archive, uploads it, and activates the deployment for the selected project environment.

Default deployment excludes tests, files prefixed with `_`, `node_modules`, `.git`, `.helmr`, `.next`, `.env`, and `.env.*`. Set `ignorePatterns` in `helmr.config.ts` to control project-specific excludes.

## Project Hint

`defineConfig` accepts an optional `project` string. The CLI uses it as the deploy target when `helmr deploy --project` is not provided.
