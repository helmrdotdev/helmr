---
title: Configuration reference
description: Task project, Workspace, image, and Run configuration.
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
| `task` | `id`, `payload`, `queue`, `maxDuration`, `ttl`, `retry`, `run` |
| `actor` | `id`, `input`, `output`, Run defaults, `run` |
| `workspace` | `image(img)`, `resources({ cpu, memory })`, `network(...)` |
| `image` | `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, `user` |
| `source` | `file(path)`, `directory(path, { ignore })` |

Workspace create requests use Secret placements:

```ts
secrets: [
  { name: "TOKEN", env: "TOKEN" },
  { name: "config-json", file: "/run/secrets/config.json" },
]
```

Secret names must match `/^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/`.
