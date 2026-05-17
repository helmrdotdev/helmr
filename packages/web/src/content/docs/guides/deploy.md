---
title: Deploy
description: Upload task source from a helmr.config.ts project.
section: Guides
sidebarLabel: Deploy
order: 310
---

# Deploy

Deploy uploads task source from a project directory that contains `helmr.config.ts`.

```sh
helmr deploy ./my-helmr-tasks
```

Use project or environment scope when needed:

```sh
helmr deploy ./my-helmr-tasks \
  --project agents \
  --environment prod
```

`helmr.config.ts` must export `defineConfig`:

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  project: "agents",
  dirs: ["./tasks"],
})
```

`dirs` is required and must contain at least one task directory. `project` is optional; `--project` overrides it for that deployment.

During deploy, the CLI:

- Loads the config.
- Indexes exported tasks from the configured directories.
- Archives the source directory.
- Creates a task deployment and prints the deployment id.

The archive always excludes `node_modules`, `.git`, `.helmr`, `.next`, `.env`, and `.env.*`. If `ignorePatterns` is not set, it also excludes tests, specs, and files that start with `_`.
