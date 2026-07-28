---
title: Deploy
description: Upload task source from a helmr.config.ts project.
section: Guides
sidebarLabel: Deploy
order: 310
---

# Deploy

Deploy uploads task source from a project directory containing `package.json`,
one exact supported lockfile, and `helmr.config.ts`. `helmr init` also creates
a safe starter `.helmrignore`.

```sh
helmr deploy ./my-helmr-tasks \
  --project agents \
  --env prod
```

`helmr.config.ts` must export `defineConfig`:

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["tasks"],
})
```

`dirs` selects source declaration directories. Project and
environment are deployment authority, not config properties. A saved login
requires both `--project` and `--env`; an environment API key derives both and
rejects those flags.

`package.json` must set `"type": "module"`, select an exact Node through
`devEngines.runtime`, select exact npm, pnpm, or Bun through `packageManager`,
and match exactly one root lockfile. Install dependencies locally before deploy
so that lockfile already exists; deploy never runs that install for you.

During deploy, the CLI:

- Applies `.helmrignore`, validates retained source and exact selectors, and
  archives the source directory.
- Sends the archive content hash with the upload metadata so the control plane can reject mismatched uploads.
- Creates a deployment, streams deployment events while the remote build runs, promotes the completed deployment by default, and prints the deployment version or ID.

`.helmrignore` is the only source-selection input; `.gitignore` is not merged.
Root `.git` is always excluded. Retained root `node_modules` and `helmr` are
rejected. Retained `.env` and `.env.*` basenames are rejected except exact
`.example`, `.sample`, and `.template` suffixes. `ignorePatterns` affects only
remote declaration discovery.

The remote build uses the selected Manager for dependency fetch and a
networkless frozen install with ordinary lifecycle semantics. It evaluates
`helmr.config.ts` once for that build attempt, compiles project and
workspace-local declaration source with the pinned Platform esbuild, and keeps
registry dependencies external in the complete installed project tree. The CLI
never executes config or declaration modules.

For automation, use JSON lines:

```sh
helmr deploy ./my-helmr-tasks --json
```

The JSON stream includes local CLI steps, deployment events, and a final deployment result. Use `--detach` to return as soon as the deployment is queued, or `--skip-promotion` to build without promoting the resulting version.
