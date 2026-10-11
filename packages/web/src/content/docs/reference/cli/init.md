---
title: helmr init
description: Create a starter Agent project.
sidebarLabel: init
---

# `helmr init`

```text
helmr init [--dir DIR] [--force]
```

Writes a starter Agent project into the current directory, or into `DIR`.

| Flag | Description |
| --- | --- |
| `--dir DIR` | Project root to initialize. Defaults to the current directory. |
| `--force` | Overwrite existing generated files. |

Without `--force`, the command fails before writing anything if any generated
file already exists.

## Generated files

| File | Contents |
| --- | --- |
| `helmr.config.ts` | `defineConfig({ dirs: ["agents"], ignorePatterns: ["**/*.test.ts"] })` |
| `.helmrignore` | Excludes `node_modules/`, `helmr/`, `dist/`, `.env` and `.env.*`, while keeping `.env*.example`, `.env*.sample` and `.env*.template`. |
| `package.json` | A private ES module package with `@helmr/sdk` as a dependency, a `packageManager` selector, and a `devEngines` entry for the matching Node.js version. |
| `tsconfig.json` | Strict `NodeNext` settings with `noEmit`, including `agents/**/*.ts`. |
| `agents/hello.ts` | One Computer definition and one Agent, both with ID `hello`. |

`agents/hello.ts`:

```ts
import { agent, computer, image } from "@helmr/sdk"

export const helloComputer = computer({
  id: "hello",
  image: image("hello").from("node:24-bookworm-slim").workdir("/app"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const hello = agent({
  id: "hello",
  computer: helloComputer,
  async turn(turn) {
    await turn.respond("Hello from Helmr")
    return { ok: true }
  },
})
```

The `@helmr/sdk` version in `package.json` matches the CLI release. Install
dependencies with your package manager before running
[`helmr build`](/docs/reference/cli/build) or
[`helmr deploy`](/docs/reference/cli/deploy). For the configuration fields, see
[Configuration](/docs/reference/config).
