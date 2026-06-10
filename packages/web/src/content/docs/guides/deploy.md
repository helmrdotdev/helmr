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
  --env prod
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

Use `--project` when automation needs to override the source-controlled project:

```sh
helmr deploy ./my-helmr-tasks \
  --project agents \
  --env prod
```

`package.json` must declare `@helmr/sdk` in `dependencies` and an explicit `packageManager`. `helmr init` creates this for new projects.

During deploy, the CLI:

- Validates `package.json` and installs missing task project dependencies locally with the declared `packageManager` so config inspection can run.
- Loads the config.
- Indexes exported tasks from the configured directories.
- Archives the source directory.
- Sends the archive content hash with the upload metadata so the control plane can reject mismatched uploads.
- Creates a deployment, streams deployment events while the remote build runs, promotes the completed deployment by default, and prints the deployment version or ID.

The archive always excludes `node_modules`, `.git`, `.helmr`, `.next`, `.env`, and `.env.*`. If `ignorePatterns` is not set, it also excludes tests, specs, and files that start with `_`.

Remote deployment builds install archived project dependencies in a product-managed build environment using the explicit `packageManager` from `package.json`. Runtime dependencies are not installed by deploy. Install them explicitly in the sandbox image build.

Use `--env-file FILE` to load local variables into the CLI process before package installation and `helmr.config.ts` inspection run. Those values are visible to child processes started by the deploy command. Values from the file do not override variables already present in the CLI process environment.

`--env-file` is for task project configuration, not Helmr CLI or runtime configuration. `HELMR_` is a reserved namespace and the deploy command rejects `HELMR_` keys in `--env-file`. Set `HELMR_API_URL` in the shell or pass `--api-url`; set `HELMR_API_KEY` in the shell or use `helmr login`.

Treat the env file as trusted project input: values are added to the deploy process environment and inherited by child processes started by deploy.

The env-file parser supports `KEY=VALUE`, `export KEY=VALUE`, single-quoted values, double-quoted values with `\n`, `\r`, `\t`, `\"`, and `\\` escapes, blank lines, whole-line comments, and unquoted trailing comments written as `KEY=value # comment`.

For automation, use JSON lines:

```sh
helmr deploy ./my-helmr-tasks --json
```

The JSON stream includes local CLI steps, deployment events, and a final deployment result. Use `--detach` to return as soon as the deployment is queued, or `--skip-promotion` to build without promoting the resulting version.
