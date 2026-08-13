---
title: Quickstart
description: Create, deploy, and run a Helmr Task in a durable Workspace.
sidebarLabel: Quickstart
---

# Quickstart

This path takes a TypeScript project from source to a completed Run. You need a
Helmr control plane with at least one active worker, plus a project and
environment you can deploy to.

## Install the CLI

```sh
curl -fsSL https://helmr.dev/install | bash
helmr login
```

Nix users can instead install the pinned package:

```sh
nix profile install github:helmrdotdev/helmr#helmr
```

## Create a project

```sh
helmr init --dir ./hello-helmr
cd ./hello-helmr
bun install
```

`helmr init` creates the Helmr config, TypeScript config, package manifest,
ignore file, and a starter declaration in `tasks/hello.ts`. The declaration
exports a Sandbox backed by a container image and a Task that returns one
result.

## Deploy it

```sh
helmr deploy . --project PROJECT --env ENVIRONMENT
```

The CLI builds a content-addressed bundle in the official local builder,
uploads it, and promotes the verified Deployment by default.

## Create a Workspace

Create a durable Workspace from the deployed `hello` Sandbox:

```sh
WORKSPACE_ID="$(helmr workspace create hello \
  --project PROJECT \
  --env ENVIRONMENT \
  --key quickstart \
  --idempotency-key quickstart:workspace)"
```

The key gives the Workspace a stable lookup value. The idempotency key makes a
retried create request safe.

## Start the Task

```sh
helmr task start hello \
  --project PROJECT \
  --env ENVIRONMENT \
  --workspace "$WORKSPACE_ID" \
  --idempotency-key quickstart:run \
  --wait
```

`--wait` waits for the Run to reach a terminal state. Without it, the command
returns after the Run is accepted.

## Inspect the Run

```sh
helmr run list --project PROJECT --env ENVIRONMENT
helmr run get RUN_ID --project PROJECT --env ENVIRONMENT
helmr run logs RUN_ID --project PROJECT --env ENVIRONMENT
helmr run events RUN_ID --project PROJECT --env ENVIRONMENT
```

Continue with [Run your first Task](/docs/guides/tutorials/first-task/) for a
typed payload and Workspace file example, or [Durable agent](/docs/guides/tutorials/durable-agent/)
to add ongoing input and output with an Actor.

## Local development note

In the Helmr repository, `make dev` starts a local database, control plane, and
web UI. It does not provide worker capacity. A remotely executed Task still
needs an active worker.
