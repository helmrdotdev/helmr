---
title: Quickstart
description: Run Helmr locally, define a task project, and start a remote run.
section: Start
sidebarLabel: Quickstart
order: 20
---

# Quickstart

Install the CLI first:

```sh
curl -fsSL https://helmr.dev/install | bash
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

This creates `package.json`, `helmr.config.ts`, and `tasks/hello.ts`. A task project must have a default-exported `defineConfig({ dirs: [...] })`; files in those directories are indexed for exported `task(...)` definitions.

## Deploy Tasks

```sh
helmr deploy .
```

Deployment indexes the configured task files, uploads a deployment-source archive, and records the current deployment for the selected project and environment.

## Start A Run

```sh
helmr run hello \
  --repo OWNER/REPO \
  --ref main \
  --payload name=Ada
```

Remote runs require a configured control plane, a worker capable of executing runs, and a GitHub workspace repository enabled for the project. Use `--subpath` when the task should run inside a repository subdirectory.

## Inspect The Run

```sh
helmr ps
helmr show RUN_ID
helmr logs RUN_ID
helmr events RUN_ID
```

If a run is waiting, resolve it with `helmr resume approve`, `helmr resume deny`, or `helmr resume message`.
