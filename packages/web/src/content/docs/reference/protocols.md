---
title: Protocols
description: Protocol messages shared by control, worker, guest runtime, and generated clients.
section: Reference
sidebarLabel: Protocols
order: 970
---

# Protocols

Protocol definitions live in `proto/` and generate Go plus TypeScript bindings.

`helmr.bundle.v0` describes deployment task bundles:

- `Bundle` combines an image, sandbox, task, and named sub-images.
- `ImageSpec` is a sequence of image steps: `from`, `run`, source copy, image copy, `workdir`, `user`, and `env`.
- `SandboxSpec` carries workspace mount and resource requests.
- `TaskSpec` carries task ID, sandbox ID, module path, export name, max duration, and secret placements.

`helmr.run.v0` describes host/guest execution:

- `RunTaskRequest` carries task ID, module path, cwd, run ID, payload JSON, secrets, and workspace overlay metadata.
- `RunEvent` carries stdout/stderr chunks, log entries, wait requests, emitted events, task output, and task completion.
- Wait messages include manual and delay waitpoints with optional timeouts.
- Checkpoint/resume messages are `SuspendForCheckpoint`, `PauseReady`, `ResumeAttach`, `ResumeDecision`, and `ResumeAck`.

Generated TypeScript protocol packages are under `proto/typescript`. Product-facing SDK APIs wrap these messages; most task authors should use `@helmr/sdk` rather than constructing protocol messages directly.
