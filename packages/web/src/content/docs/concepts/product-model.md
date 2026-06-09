---
title: Product model
description: The main Helmr objects and how they relate to each other.
section: Concepts
sidebarLabel: Product model
order: 100
---

# Product Model

Helmr organizes agent execution around projects, environments, deployments, workspaces, schedules, runs, waitpoints, and secrets.

| Object | Meaning |
| --- | --- |
| Organization | The top-level account boundary for users, API keys, projects, integrations, and workers. |
| Project | A product or work area. Projects own environments, deployments, secrets, and runs. |
| Environment | A project scope such as production, staging, or preview. Runs, secrets, and deployments are environment-scoped. |
| Worker group | A control-plane compute pool. Deployments, runtime requirements, and worker instances are routed through a worker group; new installations start with the `default` worker group. |
| Worker instance | A registered worker host that belongs to one worker group and advertises runtime capacity, labels, and protocol support. |
| Task project | A source directory with `helmr.config.ts` and TypeScript task modules. |
| Deployment | An immutable versioned upload of indexed task definitions. One current deployment pointer is used per project environment, and a deployment can contain multiple tasks. |
| Task | A TypeScript unit of work identified by `task_id`. It declares a sandbox, optional secrets, max duration, and run logic. |
| Workspace | The empty writable filesystem mounted for a run. |
| Schedule | A cron definition that creates runs for a deployed task with generated schedule metadata and stored run options. |
| Run | One execution of a deployment task with payload, task-declared secrets, workspace state, and pinned deployment metadata. |
| Waitpoint | A pause in a run for approval or operator input. |
| Secret | An encrypted value stored by name and bound to a declared task secret at run time. |

## Scope

Most operational objects are scoped to a project and environment. Deploy reads the project from `helmr.config.ts`; CLI flags such as `--project` and `--environment` select scope for commands that are not tied to a task project config.

When no explicit scope is provided, Helmr uses the default project and default environment where the API path supports it. New organizations start with `Main / Production`.

## Versioning

Helmr keeps separate version axes for separate contracts:

- API surface version: clients send `Helmr-API-Version` with a fixed date value compiled into the CLI, SDK, or console build. The control plane echoes the effective API version and rejects unsupported values.
- Deployment version: every deploy creates a new immutable code snapshot for a project environment. Content hashes remain artifact integrity metadata, but they are not used as the deployment version identity.
- Worker protocol version: workers advertise a wire protocol and supported protocol set. The control plane leases work only when the worker supports the current protocol.
- Provenance versions: deployments and runs record CLI, SDK, bundle format, and protocol metadata for debugging and audit. These values are not authorization inputs.
