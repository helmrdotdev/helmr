---
title: Runs
description: Task and Actor execution, attempts, telemetry, waits, and results.
section: Concepts
sidebarLabel: Runs
order: 160
---

# Runs

A Run is one Task or Actor execution. It records the pinned Deployment and
entrypoint, attached Workspace, payload or Actor input boundary, duration,
status, output, logs, events, metadata, and pending Wait.

A Run does not own its Workspace. The Workspace can outlive the Run.

## Statuses

| Status | Meaning |
| --- | --- |
| `queued` | The run is waiting for a worker. |
| `running` | A worker has started or is executing the run, including workspace preparation. |
| `waiting` | The task is paused for stream input, token completion, or a timer. |
| `retry-delayed` | A retry is scheduled after backoff. |
| `cancel-requested` | Cancellation is admitted and waiting for terminal convergence. |
| `succeeded` | The task completed successfully. |
| `failed` | The task failed or exceeded a limit. |
| `cancelled` | The run was cancelled. |
| `expired` | The queued run TTL expired before execution started. |
| `system-failed` | Helmr could not safely continue the Run. |

## Workspace Attachment

Every external Task start supplies an existing Workspace. Helmr validates the
deployed Task and Workspace authority before creating the Run.

Direct Workspace exec is not a Run. It is one bounded operation with a
terminal result.

## Duration

Run duration is limited by the deployed Task declaration. External starts do
not override that execution boundary.

## Attempts

A run has no attempt number while it is only queued. When a worker leases the
run, Helmr assigns the current task attempt number, starting at `1`. Worker
lease retries and queue redelivery use a separate dispatch attempt counter and
do not change the task attempt number.

The attempt number is the task execution attempt identity used by run logs,
events, and worker execution records.

## Inspecting Runs

```sh
helmr run list
helmr run get RUN_ID
helmr run logs RUN_ID
helmr run events RUN_ID
```

The SDK client can retrieve, list, wait for, and page through Run logs and
events. Logs contain stdout, stderr, and structured records. Events contain
wait decisions, metadata updates, completion, failures, queued expiry, and
cancellation. Actor progressive output is read from the Actor output channel,
not Run telemetry.
