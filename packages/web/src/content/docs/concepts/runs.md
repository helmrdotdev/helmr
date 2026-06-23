---
title: Runs
description: Execution attempts, logs, events, payloads, channel output, and task output.
section: Concepts
sidebarLabel: Runs
order: 160
---

# Runs

A run is one execution attempt for a task session. It records the pinned
deployment, pinned deployment task, task ID, payload, task-declared secret
requirements, attached workspace, max duration, status, output, logs, channel
output, events, metadata, and pending waitpoint.

A run does not own the workspace. The task session references a workspace, and
each run executes against that attached workspace.

## Statuses

| Status | Meaning |
| --- | --- |
| `queued` | The run is waiting for a worker. |
| `running` | A worker has started or is executing the run, including workspace preparation. |
| `waiting` | The task is paused at a waitpoint or time wait. |
| `succeeded` | The task completed successfully. |
| `failed` | The task failed or exceeded a limit. |
| `cancelled` | The run was cancelled. |
| `expired` | The queued run TTL expired before execution started. |

## Workspace Attachment

Starting a task creates or reuses a task session, and that session is attached to
a workspace. The run receives the workspace mount metadata and executes the task
inside the sandbox workspace path.

If no workspace is supplied, Helmr creates one using the deployed task's sandbox
definition. If a workspace is supplied, Helmr validates that the task's sandbox
is compatible with the workspace before running.

Direct workspace operations such as exec and PTY are not runs. They have their
own workspace handles and stream state.

## Duration

Run duration is limited. The default is 900 seconds, and accepted limits are 5
to 86400 seconds. A task can declare `maxDuration`; callers can also pass a max
duration option when starting a task.

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

The SDK client can retrieve, list, wait for, and stream run events. Run logs are
stored as stdout and stderr snapshots; events include logs, waitpoints and
decisions, channel output records, metadata updates, completion, failures,
queued expiry, and cancellation.
