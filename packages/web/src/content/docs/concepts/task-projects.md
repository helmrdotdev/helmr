---
title: Task projects
description: The source layout Helmr indexes and deploys.
section: Concepts
sidebarLabel: Task projects
order: 120
---

# Task Projects

A task project is the directory you deploy with `helmr deploy`. It is a package-managed TypeScript project with `package.json`, `helmr.config.ts`, and one or more task modules.

`package.json` must declare `@helmr/sdk` in `dependencies`. `helmr init` creates this structure for new task projects.

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["./tasks"],
})
```

`dirs` is required and must contain at least one directory inside the project root. Helmr discovers TypeScript and JavaScript files in those directories and indexes exported `task(...)` definitions.

## Deployment

`helmr deploy PATH` validates `package.json`, installs project dependencies for local config inspection, indexes task IDs, module paths, and export names, creates a deployment-source archive, uploads it, and activates the deployment for the selected project environment.

Workers install the archived task project's dependencies inside the Helmr-managed runtime before indexing or running tasks. This keeps top-level imports and package resolution aligned with a normal TypeScript project.

Default deployment excludes tests, files prefixed with `_`, `node_modules`, `.git`, `.helmr`, `.next`, `.env`, and `.env.*`. Set `ignorePatterns` in `helmr.config.ts` to control project-specific excludes.

## Project Hint

`defineConfig` accepts an optional `project` string. The CLI uses it as the deploy target when `helmr deploy --project` is not provided.
