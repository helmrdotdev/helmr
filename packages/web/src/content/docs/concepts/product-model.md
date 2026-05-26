---
title: Product model
description: The main Helmr objects and how they relate to each other.
section: Concepts
sidebarLabel: Product model
order: 100
---

# Product Model

Helmr organizes agent execution around projects, environments, deployments, workspaces, runs, waitpoints, and secrets.

| Object | Meaning |
| --- | --- |
| Organization | The top-level account boundary for users, API keys, projects, GitHub installations, and workers. |
| Project | A product or repository group. Projects own environments, workspace repository access, deployments, secrets, and runs. |
| Environment | A project scope such as production, staging, or preview. Runs, secrets, and deployments are environment-scoped. Worker instances provide organization-level compute capacity shared across environments. |
| Task project | A source directory with `helmr.config.ts` and TypeScript task modules. |
| Deployment | A versioned upload of indexed task definitions. One current deployment label is used per project environment, and a deployment can contain multiple tasks. |
| Task | A TypeScript unit of work identified by `task_id`. It declares a sandbox, optional secrets, max duration, and run logic. |
| Workspace | The GitHub repository, ref or SHA, and optional subpath checked out for a run. |
| Run | One execution of a deployment task against a workspace and payload. |
| Waitpoint | A pause in a run for approval or operator input. |
| Secret | An encrypted value stored by name and bound to a declared task secret at run time. |

## Scope

Most operational objects are scoped to a project and environment. Deploy reads the project from `helmr.config.ts`; CLI flags such as `--project` and `--environment` select scope for commands that are not tied to a task project config.

When no explicit scope is provided, Helmr uses the default project and default environment where the API path supports it. New organizations start with `Main / Production`.
