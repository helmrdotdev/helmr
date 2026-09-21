---
title: Runs
description: Execution records for Tasks and Actors, including attempts and telemetry.
---

# Runs

A Run is one execution of a Task or Actor. It records the pinned Deployment and
entrypoint, attached Workspace, cause, metadata, tags, attempt state, telemetry,
and terminal output or failure. Actor Runs also identify their Session.

| Status | Meaning |
| --- | --- |
| `queued` | Waiting for execution capacity. |
| `running` | Preparing or executing an attempt. |
| `waiting` | Durably parked on input, a Token, or a timer. |
| `retry_delayed` | Waiting for retry backoff. |
| `cancel_requested` | Cancellation was accepted but has not converged. |
| `succeeded` | Finished with an output. |
| `failed` | Application execution failed. |
| `cancelled` | Cancellation reached a terminal state. |
| `expired` | Queued TTL elapsed before execution. |
| `system_failed` | Helmr could not safely continue execution. |

A Run is pinned to the Deployment chosen at start. Promoting newer code does
not rewrite the existing Run's authority. Its Workspace is a separate durable
resource and can survive the Run.

An attempt number begins when a worker leases execution. Retries may increment
the task attempt while dispatch redelivery remains a separate internal
mechanism. Log and event records include attempt provenance so repeated
execution is observable.

Run logs contain stdout, stderr, and structured log records. Run events capture
lifecycle and wait decisions. Actor output is not telemetry and belongs to the
Session output log.

The client can retrieve and list Runs, request cancellation, wait for a typed
handle, and page logs or events. Waiting returns a success/failure result;
`.unwrap()` returns output or throws the Run failure. Cancellation is a request
for convergence, not proof that the workload stopped at the instant of the API
response.

## Loss of an executing environment

If Helmr loses an execution lease after the Run has started, the environment may
contain changes that have not been saved. Helmr records the Run failure and marks
the Computer as requiring recovery, preserving its last published disk version.
It does not silently start a replacement attempt from that older version, even
when Task retries are enabled. The original lease-loss or execution-deadline
reason remains on the Run.

Owned Runs sharing that Computer also fail with `computer_recovery_required`;
Helmr does not resume a parked parent from an older disk or memory checkpoint.
The Run that lost its lease retains its original failure reason.

A launch or checkpoint restore that has not started execution can still be
retried. Once restored execution starts, losing it requires recovery even if its
resume acknowledgement has not reached the Control Plane. A failure record alone
is not proof that physical cleanup has finished.
