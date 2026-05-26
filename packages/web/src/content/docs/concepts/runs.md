---
title: Runs
description: Execution state, logs, events, payloads, and outputs.
section: Concepts
sidebarLabel: Runs
order: 160
---

# Runs

A run is one execution of a deployment task in a project environment. It records the pinned deployment, pinned deployment task, task ID, payload, secret bindings, workspace artifact, max duration, status, output, logs, events, and pending waitpoint.

## Statuses

| Status | Meaning |
| --- | --- |
| `queued` | The run is waiting for a worker. |
| `running` | A worker has started or is executing the run. |
| `checkpointing` | The worker is snapshotting runtime state for a waitpoint. |
| `waiting` | The task is paused at a waitpoint. |
| `succeeded` | The task completed successfully. |
| `failed` | The task failed or exceeded a limit. |
| `cancelled` | The run was cancelled. |

## Duration

Run duration is limited. The default is 900 seconds, and accepted limits are 5 to 86400 seconds. A task can declare `maxDuration`; the CLI can also pass `--max-duration-seconds`.

## Inspecting Runs

```sh
helmr ps
helmr show RUN_ID
helmr logs RUN_ID
helmr events RUN_ID
```

The SDK client can retrieve, list, wait for, and stream run events. Run logs are stored as stdout and stderr snapshots; events include logs, waitpoint requests and decisions, emitted task events, completion, failures, timeouts, and cancellation.
