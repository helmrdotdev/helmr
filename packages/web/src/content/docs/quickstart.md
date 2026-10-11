---
title: Quickstart
description: Create, deploy, and start a Helmr Agent.
sidebarLabel: Quickstart
---

# Quickstart

This path takes a TypeScript project from source to a completed Turn. You need a
Helmr control plane with at least one active worker, plus a project and
environment you can deploy to.

## Install the CLI

```sh
curl -fsSL https://helmr.dev/install | bash
helmr login
```

The installer selects a completed stable release for your OS and architecture.
To install a specific published preview, pass its full release tag:

```sh
curl -fsSL https://helmr.dev/install | bash -s -- --version "$HELMR_RELEASE_TAG"
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
exports a Computer recipe and an Agent that stages a greeting and returns a
machine result.

## Deploy it

```sh
helmr deploy . --project PROJECT --env ENVIRONMENT
```

The CLI builds a content-addressed bundle in the official local builder,
uploads it, and promotes the verified Deployment by default.

## Start the Agent

```sh
helmr agent start hello \
  --project PROJECT \
  --env ENVIRONMENT \
  --input-json '[]' \
  --idempotency-key quickstart:hello \
  --json
```

The receipt contains `session_id` and `turn_id`. Helmr creates the Computer from
the Agent's definition. Admission means the input was accepted; execution and
saving can still be pending. Retry an uncertain request with the same key and
arguments.

## Observe the Turn

Use the IDs from the receipt:

```sh
helmr session events SESSION_ID --project PROJECT --env ENVIRONMENT
helmr session turn wait SESSION_ID TURN_ID --timeout 10m --project PROJECT --env ENVIRONMENT --json
helmr session turn get SESSION_ID TURN_ID --project PROJECT --env ENVIRONMENT --json
```

Read events while work proceeds. The bounded wait returns either a settled
outcome or timeout; timeout does not cancel work. Completed includes the committed
machine result and staged human response after required Save publication.

## Continue or close the Session

```sh
helmr session enqueue SESSION_ID --input-json '[]' --project PROJECT --env ENVIRONMENT
helmr session close SESSION_ID --project PROJECT --env ENVIRONMENT
```

Enqueue adds another Turn. Close rejects new ordinary admission and drains work
already accepted. Continue with the [Agent reference](/docs/reference/sdk/agents-and-sessions).

## Local development note

In the Helmr repository, `make dev` starts a local database, control plane, and
web UI. It uses the S3 object stores named by `CAS_URI` and `PLATFORM_STORE_URI`
and does not provide worker capacity. A remotely executed Agent still needs an
active worker.
