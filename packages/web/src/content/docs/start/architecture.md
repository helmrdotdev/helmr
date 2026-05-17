---
title: Architecture
description: The runtime components and execution flow behind a Helmr run.
section: Start
sidebarLabel: Architecture
order: 30
---

# Architecture

Helmr is split between authoring tools, a control plane, and workers.

| Component | Role |
| --- | --- |
| TypeScript SDK | Declares task projects, tasks, images, sandboxes, secrets, resources, workspaces, and waitpoints. |
| CLI | Logs in, deploys task source, starts runs, manages secrets, reads logs and events, and resolves waitpoints. |
| Control plane | Stores projects, environments, deployments, runs, events, logs, secrets, API keys, workers, and waitpoints. |
| Worker | Claims queued runs, prepares task source and workspace checkout, starts the guest, streams logs, and releases results. |
| Guest runtime | Loads the deployed task module inside the guest and bridges task output, logs, events, and waitpoint requests. |

## Run Flow

1. A task project is deployed from a directory containing `helmr.config.ts`.
2. The control plane stores the task-source artifact and marks the deployment active for a project environment.
3. A run is created for a deployed `task_id`, payload, secret bindings, and GitHub workspace.
4. A worker claims the run and receives the resolved task source, workspace source, secrets, and duration limit.
5. The worker starts an isolated Linux guest, checks out the workspace, injects secrets, and runs the TypeScript task.
6. Logs, events, output, failures, and waitpoints stream back to the control plane.
7. Terminal runs finish as `succeeded`, `failed`, or `cancelled`.

## Isolation Boundary

Workers execute task code inside Firecracker-backed Linux guests. The workspace is mounted in the sandbox, task-declared secrets are injected at run time, and checkpoint artifacts are encrypted before leaving the worker staging directory.
