---
title: Deploy
description: Upload task source from a helmr.config.ts project.
section: Guides
sidebarLabel: Deploy
order: 310
---

# Deploy

Deploy uploads task source from a project directory that contains `package.json` and `helmr.config.ts`.

```sh
helmr deploy ./my-helmr-tasks
```

Use an environment scope when needed:

```sh
helmr deploy ./my-helmr-tasks \
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

`project` and `dirs` are required. `project` selects the deploy target from source-controlled config; `dirs` must contain at least one task directory.

`package.json` must declare `@helmr/sdk` in `dependencies` and an explicit `packageManager`. `helmr init` creates this for new projects.

During deploy, the CLI:

- Validates `package.json` and installs missing task project dependencies locally with the declared `packageManager` so config inspection can run.
- Loads the config.
- Indexes exported tasks from the configured directories.
- Archives the source directory.
- Sends the archive content hash with the upload metadata so the control plane can reject mismatched uploads.
- Creates a deployment and prints the deployment ID.

The archive always excludes `node_modules`, `.git`, `.helmr`, `.next`, `.env`, and `.env.*`. If `ignorePatterns` is not set, it also excludes tests, specs, and files that start with `_`.

Remote deployment builds install archived project dependencies in a product-managed build environment using the explicit `packageManager` from `package.json`. Runtime dependencies are not installed by deploy. Install them explicitly in the sandbox image build.
