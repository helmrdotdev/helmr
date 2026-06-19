---
title: Protocols
description: Protocol messages shared by control, worker, guest runtime, and generated clients.
section: Reference
sidebarLabel: Protocols
order: 970
---

# Protocols

Protocol definitions live in `proto/` and generate Go plus TypeScript bindings.

The control-plane REST API version is date based. Worker and bundle protocols are not: they use explicit protocol identifiers so data-plane compatibility is visibly separate from request/response API compatibility.

`helmr.bundle.v0` describes deployment task bundles:

- `Bundle` combines an image, sandbox, task, and named sub-images.
- `ImageSpec` is a sequence of image steps: `from`, `run`, source copy, image copy, `workdir`, `user`, and `env`.
- `SandboxSpec` carries workspace mount and resource requests.
- `TaskSpec` carries task ID, sandbox ID, module path, export name, max duration, secret placements, and declarative schedule specs.
- `TaskScheduleSpec` carries cron, timezone, and active-state metadata for deployment-owned schedules.

`helmr.run.v0` describes host/guest execution:

- `RunTaskRequest` carries task ID, module path, cwd, run ID, task session ID, payload JSON, secrets, and workspace session context.
- `RunEvent` carries stdout/stderr chunks, log entries, waitpoint requests, channel output append notifications, metadata update notifications, task output, and task completion.
- Waitpoint requests cover external completion and time-based sleeps with optional timeouts.
- Checkpoint/resume messages are `SuspendForCheckpoint`, `PauseReady`, `ResumeAttach`, `ResumeDecision`, and `ResumeAck`.

`helmr.worker.v0` is the current control-plane to worker lease protocol. Workers send `protocol_version` and `worker_version` in their capabilities. The control plane requires the current worker protocol before it records heartbeat state or leases execution/build work, and lease payloads include the selected protocol version for the worker to parse.

Deployment builds emit bundle format version `1`. Deployment tasks carry this value so future bundle readers can reject or branch on unsupported formats before trying to execute task code.

Generated TypeScript protocol packages are under `proto/typescript`. Product-facing SDK APIs wrap these messages; most task authors should use `@helmr/sdk` rather than constructing protocol messages directly.
