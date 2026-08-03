---
title: Product model
description: The main Helmr objects and how they relate to each other.
section: Concepts
sidebarLabel: Product model
order: 100
---

# Product model

Helmr combines immutable deployed Programs with durable Workspaces and Runs.

```text
Deployment
  -> Task, Actor, Workspace, and Schedule declarations

Workspace
  -> committed filesystem state
  -> Task and Actor execution
  -> bounded direct exec

Task start
  -> Run
  -> attempts, logs, events, waits, and result

Actor
  -> stable ID or key
  -> fixed input and output channels
  -> continuation Runs
```

| Object | Meaning |
| --- | --- |
| Organization | Account and authorization boundary. |
| Project / Environment | Deployment, Secret, Workspace, Actor, Token, and Run scope. |
| Deployment | Immutable Program Artifact plus indexed declarations. |
| Task | One-shot typed workflow definition. |
| Actor | Stable workflow identity with fixed durable input and output channels. |
| Workspace | Durable filesystem and execution-state object. |
| Run | One Task or Actor execution with attempts, telemetry, waits, and a terminal result. |
| Token | Independent durable external-completion primitive; multiple Runs in one Environment may wait on it. |
| Schedule | Source-declared cron trigger reconciled by Deployment promotion. |
| Secret | Encrypted, versioned value referenced by declaration. |

Task start requires an existing Workspace and creates a Run directly. There is
no mandatory Session wrapper. Stable interactive workflows use Actors; one-shot
external approvals use Tokens.

Most operational objects are project-and-environment scoped. API keys are
already bound to that scope. Saved user login calls use explicit project and
environment route prefixes.

API contract version, Deployment version, worker protocol version, and client
provenance are separate axes. They are compatibility and diagnostic metadata,
not application authorization inputs.
