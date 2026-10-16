---
title: helmr init
description: Create a starter Agent project.
sidebarLabel: init
---

# `helmr init`

```text
helmr init [--dir DIR] [--force]
helmr init --template TEMPLATE [--provider PROVIDER] [--name NAME] [--dir DIR] [--force]
```

Writes a starter Agent project into the current directory, or into `DIR`.
Without `--template`, it writes the minimal `hello` project described below.

| Flag | Description |
| --- | --- |
| `--dir DIR` | Project root to initialize. Defaults to the current directory. |
| `--force` | Overwrite existing generated files. |
| `--template TEMPLATE` | Write a conversational Agent built on a coding-agent harness. Available: `claude-code`. |
| `--provider PROVIDER` | Model provider for the template. Defaults to the template's first provider. |
| `--name NAME` | Agent name: lowercase letters, digits and hyphens, starting with a letter, up to 40 characters. Defaults to a name picked at random from a list of scientists and mathematicians. |

Without `--force`, the command fails before writing anything if any generated
file already exists.

## Generated files

| File | Contents |
| --- | --- |
| `helmr.config.ts` | `defineConfig({ dirs: ["agents"], ignorePatterns: ["**/*.test.ts"] })` |
| `.helmrignore` | Excludes `node_modules/`, `helmr/`, `dist/`, `.env` and `.env.*`, while keeping `.env*.example`, `.env*.sample` and `.env*.template`. |
| `package.json` | A private ES module package with `@helmr/sdk` as a dependency, a `packageManager` selector, and a `devEngines` entry for the matching Node.js version. |
| `tsconfig.json` | Strict `NodeNext` settings with `noEmit`, including `agents/**/*.ts`. |
| `agents/hello.ts` | One Template definition and one Agent, both with ID `hello`. |

`agents/hello.ts`:

```ts
import { agent, template, image } from "@helmr/sdk"

export const helloComputer = template({
  id: "hello",
  image: image("hello").from("node:24-bookworm-slim").workdir("/app"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const hello = agent({
  id: "hello",
  template: helloComputer,
  async turn(turn) {
    await turn.respond("Hello from Helmr")
    return { ok: true }
  },
})
```

## Templates

A template project is a working conversational Agent: each Helmr Session keeps
one harness conversation, and the harness's questions and tool approvals
become Helmr questions.

| Template | Provider | Model | Secret | Sent only to |
| --- | --- | --- | --- | --- |
| `claude-code` | `anthropic` | `claude-sonnet-5-5` | `ANTHROPIC_API_KEY` | `https://api.anthropic.com` |

The command refuses a directory whose `agents/` holds other `.ts` files, even
with `--force`, since they would deploy beside the new Agent. The command prints the Agent name, the model and the Secret to add. Add that
Secret in the Console for each Environment you deploy to; the template binds
it in protected mode, so the Computer only sees a placeholder.

| File | Contents |
| --- | --- |
| `agents/settings.ts` | The Agent name, a one-line introduction used as an addition to the system prompt, the model and the Secret binding. |
| `agents/NAME.ts` | The Template and the Agent, both with ID `NAME`. |
| Bridge files in `agents/` | The code that connects the harness to Helmr Sessions and Turns. |
| `README.md`, `helmr.config.ts`, `.helmrignore`, `tsconfig.json`, `package.json` | Project files; `helmr.config.ts` lists the harness package in `build.external`. |

The `@helmr/sdk` version in `package.json` matches the CLI release. Install
dependencies with your package manager before running
[`helmr build`](/docs/reference/cli/build) or
[`helmr deploy`](/docs/reference/cli/deploy). For the configuration fields, see
[Configuration](/docs/reference/config).
