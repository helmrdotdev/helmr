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

This creates `package.json`, `helmr.config.ts`, and `tasks/hello.ts`. A task project must have a default-exported `defineConfig({ project, dirs: [...] })`; files in those directories are indexed for exported `task(...)` definitions.

## Deploy Tasks

```sh
helmr deploy .
```

Deployment indexes the configured task files, uploads a content-hashed deployment-source archive, and records the current deployment for the configured project and selected environment.

## Start A Task

```sh
helmr session start hello \
  --payload name=Ada
```

Remote session starts require a configured control plane and a worker capable of
executing the task. If no workspace is supplied, Helmr creates a durable
workspace using the deployed task's sandbox and attaches the new session to
it.

## Inspect The Run

```sh
helmr run list
helmr run get RUN_ID
helmr run logs RUN_ID
helmr run events RUN_ID
```

If a task is waiting for human input, append to the session input stream from a
trusted app, webhook handler, or operator tool:

```ts
await client.sessions.open(sessionId).input("approval").send({
  decision: "approve",
})
```

Use an external callback token instead when the integration should complete a
one-shot callback URL, such as an email link or third party webhook.
