---
title: Product model
description: The main Helmr objects and how they relate to each other.
section: Concepts
sidebarLabel: Product model
order: 100
---

# Product Model

Helmr is organized around durable workspaces and task sessions that attach to
them.

The workspace is the lasting product object: it holds filesystem identity,
current persisted state, live materialization state, and direct operations such
as exec and PTY. A task session is a task invocation history attached to a
workspace. A run is one execution attempt for that session.

```text
workspace
  -> materialization
  -> exec handles
  -> pty sessions
  -> related task sessions

task
  -> sandbox definition
  -> payload schema
  -> run function

task session
  -> task invocation history
  -> channel records
  -> runs
  -> attached workspace

run
  -> one execution attempt
  -> logs, events, waitpoints, output
  -> attached workspace
```

## Objects

| Object | Meaning |
| --- | --- |
| Organization | The top-level account boundary for users, API keys, projects, environments, workers, and workspaces. |
| Project | A product or work area. Projects own environments, deployments, secrets, schedules, workspaces, task sessions, and runs. |
| Environment | A project scope such as production, staging, or preview. Deployments, secrets, workspaces, sessions, and runs are environment-scoped. |
| Worker group | A compute pool. Deployments, build leases, materializations, and run leases are routed through a worker group; new installations start with `default`. |
| Worker instance | A registered worker host that advertises runtime capacity, labels, and protocol support. |
| Task project | A source directory with `helmr.config.ts` and TypeScript task modules. |
| Deployment | An immutable uploaded snapshot of indexed task and sandbox definitions for a project environment. |
| Task | A TypeScript work harness identified by task ID. It declares a sandbox, optional secrets, optional payload schema, and run logic. |
| Sandbox | A low-level execution environment definition: image, workspace mount path, resources, and network policy. |
| Workspace | A durable filesystem/work-state object. It can outlive a task session and be used by direct workspace operations. |
| Materialization | A live worker/VM instance for a workspace. Direct exec, PTY, and task runs use materializations when they need live execution. |
| Exec | A durable command handle created directly on a workspace. It records command state and stream cursors. |
| PTY session | A durable interactive terminal handle created directly on a workspace. It records terminal state and stream cursors. |
| Task session | One task invocation and its interaction history. It has channel records and ordered runs, and it references a workspace. |
| Run | One execution attempt for a task session. It records pinned deployment metadata, logs, events, waitpoints, and output. |
| Schedule | A cron definition that starts task sessions for a deployed task. |
| Waitpoint | A durable pause in a run for time or external completion. |
| Waitpoint token | A scoped capability that can complete one token waitpoint. |
| Session channel | A named durable input or output lane owned by a task session. |
| Secret | An encrypted value stored by name and bound to a declared task secret at run time. |

## Workspace First

Starting a task without a workspace creates a workspace and attaches the new
task session to it. Starting with an existing workspace attaches the new task
session to that workspace after sandbox compatibility is checked.

Direct workspace operations do not create task sessions:

- opening or retrieving a workspace
- materializing, connecting, or stopping a workspace
- creating workspace exec handles
- creating workspace PTY sessions
- reading exec or PTY streams

This keeps task history and workspace state separate. Sessions answer "what task
conversation or workflow happened?" Workspaces answer "what filesystem/work
state exists now, and what live operations can I perform on it?"

## Scope

Most operational objects are scoped to a project and environment. Deploy reads
the project from `helmr.config.ts` by default. Interactive clients can use the
selected UI or CLI scope, while API clients either send explicit `project_id`
and `environment_id` parameters or use an API key bound to one project
environment.

New organizations start with `Main / Production`.

## Versioning

Helmr keeps separate version axes for separate contracts:

- API surface version: clients send `Helmr-API-Version` with a fixed date value
  compiled into the CLI, SDK, or console build. The control plane echoes the
  effective API version and rejects unsupported values.
- Deployment version: every deploy creates a new immutable code snapshot for a
  project environment.
- Worker protocol version: workers advertise their active wire protocol. The
  control plane leases work only when the worker protocol matches the
  deployment's required protocol.
- Provenance versions: deployments and runs record CLI, SDK, bundle format, and
  protocol metadata for debugging and audit.

These version fields are diagnostic and compatibility metadata. They are not
authorization inputs.
