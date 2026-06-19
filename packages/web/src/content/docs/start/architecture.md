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
| TypeScript SDK | Declares task projects, tasks, schedules, images, sandboxes, secrets, resources, workspaces, session channels, metadata, waits, waitpoint tokens, and logs. |
| CLI | Logs in, deploys task source, starts runs, manages secrets, reads logs and events, and resolves waitpoints. |
| Control plane | Stores projects, environments, deployments, schedules, sessions, runs, events, logs, channel records, metadata, secrets, API keys, workers, and waitpoints. |
| Dispatcher | Reconciles queued runs, repairs schedule next-fire entries into Redis, starts scheduled task sessions, and sweeps expired executions. |
| Worker | Leases queued runs, prepares task source and the workspace volume, starts the guest, streams logs, and releases results. |
| Guest runtime | Loads the deployment task module inside the guest and bridges task output, logs, session channel output, metadata updates, waits, and waitpoints. |

Workers register into a worker group. The initial control plane creates a `default` worker group and routes deployments, build leases, and run session leases through that group.

## Deployment Model

Helmr uses the same control-plane architecture for managed cloud and self-hosted
deployments. Organizations are the top-level tenant boundary, and the runtime,
worker, dispatcher, database, API, and task execution paths are designed around
that model.

Managed cloud can create many organizations. Self-hosted deployments run the
same architecture with initial setup gated to one organization. The difference is
at the organization creation boundary, not in the runtime or worker session
model.

## Run Flow

1. A task project is deployed from a directory containing `helmr.config.ts`.
2. The control plane stores the deployment-source artifact and marks the deployment active for a project environment.
3. A task start or scheduled fire creates a task session and a current run from the stored task, run options, and generated schedule metadata payload.
4. A worker in the matching worker group leases the run and receives the resolved task source, workspace mount metadata, secrets, and duration limit.
5. The worker starts an isolated Linux guest, materializes the workspace volume, injects secrets, and runs the TypeScript task.
6. Logs, events, output, channel records, metadata updates, failures, and waitpoints stream back to the control plane.
7. Terminal runs finish as `succeeded`, `failed`, or `cancelled`.

## Isolation Boundary

Workers execute task code inside Firecracker-backed Linux guests. The workspace is mounted in the sandbox, task-declared secrets are injected at run time, and checkpoint artifacts are encrypted before leaving the worker staging directory.

The task image supplies user tools and dependencies. Helmr supplies the runtime substrate around that image, including guest boot, runtime filesystems, DNS setup, hostname setup, logs, session channels, waitpoints, and timeout enforcement. See [Runtime environment](/docs/concepts/runtime-environment/) for the task-visible contract.
