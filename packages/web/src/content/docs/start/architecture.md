---
title: Architecture
description: The runtime components behind Helmr workspaces, sessions, and runs.
section: Start
sidebarLabel: Architecture
order: 30
---

# Architecture

Helmr is split between authoring tools, a control plane, and workers.

| Component | Role |
| --- | --- |
| TypeScript SDK | Declares task projects, tasks, schedules, images, sandboxes, secrets, resources, session streams, waits, callback tokens, metadata, and logs. The runtime client starts sessions and opens workspaces. |
| CLI | Logs in, deploys task source, starts tasks, manages sessions and run attempts, operates workspaces, manages secrets, and inspects session/run state. |
| Control plane | Stores projects, environments, deployments, schedules, sessions, runs, workspaces, materializations, execs, PTYs, events, logs, stream records, metadata, secrets, API keys, workers, and waits. |
| Dispatcher | Reconciles queued runs, repairs schedule next-fire entries into Redis, starts scheduled sessions, and sweeps expired executions. |
| Worker | Leases queued runs, materializes workspaces, starts isolated guests, runs task code, serves direct workspace exec and PTY requests, streams logs, and releases results. |
| Guest runtime | Loads the deployment task module inside the guest and bridges task output, logs, session stream output, metadata updates, waits, exec streams, and PTY streams. |

Workers enroll into explicitly configured worker groups. Run and build groups use the same identity and lifecycle model but scale independently. Enrollment proves AWS instance identity and binds the issued credential to the group's account, region, Auto Scaling group, instance profile, AMI policy, and permitted role.

## Deployment Model

Helmr uses the same control-plane architecture for managed cloud and self-hosted
deployments. Organizations are the top-level tenant boundary, and the runtime,
worker, dispatcher, database, API, and task execution paths are designed around
that model.

Managed cloud can create many organizations. Self-hosted deployments run the
same architecture with initial setup gated to one organization. Deployment mode
is an edge policy used for organization setup and future commercial policy such
as billing; it does not branch worker enrollment, scheduling, runtime, or storage.

## Run Flow

1. A task project is deployed from a directory containing `helmr.config.ts`.
2. The control plane stores the deployment-source artifact and marks the deployment active for a project environment.
3. A session start or scheduled fire creates or reuses a session, creates a run attempt, and attaches a workspace.
4. If no workspace is supplied, Helmr creates one from the deployed task's sandbox. If a workspace is supplied, Helmr validates that it matches the task's sandbox.
5. A worker in the matching worker group leases the run and receives the resolved task source, workspace mount metadata, secrets, and duration limit.
6. The worker starts an isolated Linux guest, materializes the workspace, injects task-declared secrets, and runs the TypeScript task.
7. Logs, events, output, stream records, metadata updates, failures, and waits stream back to the control plane.
8. Terminal runs finish as `succeeded`, `failed`, or `cancelled`. The attached workspace can outlive the run.

## Workspace Flow

Workspace APIs operate on the durable workspace directly. Opening, retrieving,
updating, materializing, connecting, stopping, deleting, creating execs, creating
PTYs, and reading workspace streams do not create sessions.

Direct execs and PTYs use the workspace sandbox and filesystem state. They are
useful for operator inspection, setup commands, and interactive work that should
not be modeled as a task run.

## Isolation Boundary

Workers execute task code and direct workspace operations inside
Firecracker-backed Linux guests. The workspace is mounted in the sandbox,
task-declared secrets are injected only for task runs, and checkpoint artifacts
are encrypted before leaving the worker staging directory.

The task image supplies user tools and dependencies. Helmr supplies the runtime
substrate around that image, including guest boot, runtime filesystems, DNS
setup, hostname setup, logs, session streams, workspace streams, waits,
and timeout enforcement. See [Runtime environment](/docs/concepts/runtime-environment/)
for the task-visible contract.
