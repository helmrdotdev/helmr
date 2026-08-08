---
title: How Helmr works
description: The core resources and lifecycle behind deployed durable programs.
sidebarLabel: How Helmr works
---

# How Helmr works

Helmr deploys JavaScript and TypeScript programs, runs them in isolated Linux
sandboxes, and keeps execution state in explicit durable resources.

```text
Project + Environment
  -> current immutable Deployment
  -> Task, Actor, Sandbox, and Schedule declarations

Sandbox -> Workspace -> Task or Actor Run
Actor Run <-> Session input and output
Run -> attempts, logs, events, waits, and terminal result
```

A **Deployment** is an immutable build of one project. Promotion makes its
declarations current in an Environment. A **Sandbox** declaration selects an
image and resources. Creating from it produces a durable **Workspace** with a
committed filesystem and fixed Secret placements.

A **Task** is bounded one-shot work. Starting it with a Workspace creates a
**Run**. An **Actor** adds a stable **Session** for ordered input and output;
the work that handles those messages still occurs in managed Runs. A Run can
park durably while waiting for Actor input, a Token, or a timer.

Resources are addressed by IDs. Optional keys on Workspaces and Sessions are
stable collection lookups, not substitutes for resource IDs in every API.
Idempotency keys make create, start, input, and mutation requests safe to retry.

The `/v1` developer API and `HelmrClient` expose the same main lifecycle:
definitions are read from the current or selected Deployment, Workspaces are
created from Sandbox IDs, Tasks and Actors start against Workspace references,
and Runs, Sessions, Schedules, and Secrets have dedicated read surfaces.

Helmr separates application state from diagnostics. Workspace files and Actor
output are durable application surfaces. Run logs and events explain execution.
Payload and metadata are persisted operational data, so credentials belong in
Secrets rather than either surface.
