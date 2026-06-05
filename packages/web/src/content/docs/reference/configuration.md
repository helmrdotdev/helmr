---
title: Configuration reference
description: Task project, sandbox, image, resource, and secret configuration.
section: Reference
sidebarLabel: Configuration
order: 930
---

# Configuration reference

## Task project

A task project must contain `package.json` and `helmr.config.ts`. `package.json` must declare `@helmr/sdk` in `dependencies` and an explicit `packageManager`.

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  project: "my-project",
  dirs: ["./tasks"],
  ignorePatterns: ["**/*.test.*"],
})
```

`dirs` is required and must be non-empty. Deploy excludes `node_modules`, `.git`, `.helmr`, `.next`, `.env`, and `.env.*`; when `ignorePatterns` is omitted, tests, specs, and underscore-prefixed files are also excluded.

## Runtime configuration

| Surface | Fields |
| --- | --- |
| `task` | `id`, `sandbox`, `maxDuration`, `secrets`, `run` |
| `sandbox` | `image(img)`, `workspace(mountPath)`, `resources({ cpu, memory })` |
| `image` | `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, `user` |
| `source` | `file(path)`, `directory(path, { ignore })` |

Secret declarations use placements:

```ts
secrets: [
  { name: "TOKEN", env: "TOKEN" },
  { name: "config-json", file: "/run/secrets/config.json", mode: "0400" },
  { name: "creds", dir: "/run/secrets/creds", mode: "0700" },
]
```

Secret names must match `/^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/`.
