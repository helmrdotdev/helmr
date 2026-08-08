---
title: Deploy a project
description: Validate, upload, build, and promote a Helmr project with the CLI.
---

# Deploy a project

From a directory containing `helmr.config.ts`, `package.json`, exactly one
supported lockfile, and declaration source, run:

```sh
helmr deploy . --project agents --env production
```

Saved-login commands require `--project` and `--env`. An environment API key is
already scoped and rejects those flags. By default the CLI waits for the remote
build and promotes the completed Deployment.

`helmr.config.ts` selects declaration directories:

```ts
import { defineConfig } from "@helmr/sdk"
export default defineConfig({ dirs: ["tasks", "actors"] })
```

The CLI applies `.helmrignore`, archives the retained source, uploads its
content hash, creates a Deployment, and follows build events. It does not run a
local dependency install or execute the config. Keep the selected package
manager version and root lockfile exact and consistent before deploying.

Useful modes:

```sh
helmr deploy . --project agents --env staging --json
helmr deploy . --project agents --env staging --skip-promotion
helmr deploy . --project agents --env staging --detach
```

`--json` emits JSON lines for automation. `--skip-promotion` retains a built
Deployment without making it current. `--detach` returns after queuing and does
not promote. Use `--no-image-cache` when diagnosing a Workspace image build.

`.helmrignore` is the submitted-source boundary; `.gitignore` is not merged.
Do not retain secrets or `.env` files in the archive. Put runtime credentials
in Helmr Secrets instead.
