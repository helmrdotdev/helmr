---
title: Quickstart
description: Run Helmr locally, define a task project, and start work in a durable workspace.
section: Start
sidebarLabel: Quickstart
order: 20
---

# Quickstart

Install the CLI first:

```sh
curl -fsSL https://helmr.dev/install | bash
```

Nix users can install the CLI from the project flake:

```sh
nix profile install github:helmrdotdev/helmr#helmr
```

Use Nix when possible so Go, Bun, Buf, PostgreSQL, and infrastructure tooling match CI.

```sh
nix develop
nix run .#doctor
```

## Start Local Services

```sh
make dev
```

The dev stack starts a disposable PostgreSQL database when `HELMR_DATABASE_URL` is not set, runs the control plane, and serves the local web UI at:

```text
http://127.0.0.1:3000/dev/login
```

Use that URL to create a local owner session and inspect seeded runs.

## Create A Task Project

```sh
helmr init
```

This creates `.helmrignore`, `package.json`, `tsconfig.json`,
`helmr.config.ts`, and `tasks/hello.ts`. Install the declared dependencies once
to create the exact lockfile. `helmr.config.ts` selects `tasks` as the source
declaration boundary; deploy compiles those modules remotely.

## Deploy Tasks

```sh
helmr deploy . --project PROJECT --env ENVIRONMENT
```

Deployment uploads a content-hashed source archive, builds and discovers the
project remotely, and records the immutable Program for the explicit project
and environment.

## Start A Task

```sh
WORKSPACE_ID="$(helmr workspace create hello \
  --key quickstart \
  --idempotency-key quickstart-workspace)"

helmr task start hello \
  --workspace "${WORKSPACE_ID}" \
  --idempotency-key quickstart-run
```

Remote Task starts require a configured control plane, a deployed Workspace
declaration, and an active worker.

## Inspect The Run

```sh
helmr run list
helmr run get RUN_ID
helmr run logs RUN_ID
helmr run events RUN_ID
```

For one external approval, create a Token in Task code and complete it from a
trusted backend:

```ts
await client.tokens.complete(tokenId, {
  result: { decision: "approve" },
  idempotencyKey: "approval:123",
})
```

Use an Actor's fixed input channel for continuing multi-message workflows.
