---
title: Architecture
description: The runtime components behind Helmr Workspaces, Actors, and Runs.
section: Start
sidebarLabel: Architecture
order: 30
---

# Architecture

Helmr is split between authoring tools, a control plane, and workers.

| Component | Role |
| --- | --- |
| TypeScript SDK | Declares Tasks, Actors, Workspaces, source Schedules, Secrets, waits, Tokens, metadata, and logs. |
| CLI | Logs in, deploys source, starts Tasks, inspects Runs, and operates supported Workspace and Secret surfaces. |
| Control plane | Stores Deployments, Schedules, Actors, Runs, Workspaces, records, events, logs, metadata, Secrets, API keys, workers, and waits. |
| Dispatcher | Admits queued Runs, binds pending Schedule Workspaces, fires Schedule cursors, and sweeps expired execution authority. |
| Worker | Leases queued Runs, materializes Workspaces, starts isolated guests, runs Task code, serves bounded Workspace exec requests, records logs, and releases results. |
| Guest runtime | Loads the immutable Program inside the guest and bridges Task results, Actor input/output, logs, metadata updates, waits, and internal Process I/O. |

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
3. A Task start, Actor continuation, or Schedule fire creates a Run and attaches an explicit Workspace.
4. Helmr validates the explicitly supplied Workspace and its deployed declaration.
5. A worker in the matching worker group leases the run and receives the resolved task source, workspace mount metadata, secrets, and duration limit.
6. The worker starts an isolated Linux guest, materializes the workspace, injects task-declared secrets, and runs the TypeScript task.
7. Logs, events, Task result or Actor records, metadata updates, failures, and waits return to the control plane.
8. Terminal runs finish as `succeeded`, `failed`, or `cancelled`. The attached workspace can outlive the run.

## Workspace Flow

Workspace APIs operate on the durable Workspace directly. Create, retrieve,
committed file reads, bounded exec, and delete do not create Runs.

Direct exec uses the Workspace sandbox and filesystem state. It is useful for
bounded setup and inspection that should not be modeled as a Task Run.

## Isolation Boundary

Workers execute task code and direct workspace operations inside
Firecracker-backed Linux guests. The workspace is mounted in the sandbox,
task-declared secrets are injected only for task runs, and checkpoint artifacts
are encrypted before leaving the worker staging directory.

The Workspace image supplies user tools. Helmr supplies the runtime substrate
around it, including guest boot, runtime filesystems, DNS setup, hostname
setup, logs, Actor channels, waits, and timeout enforcement. See
[Runtime environment](/docs/concepts/runtime-environment/)
for the task-visible contract.
