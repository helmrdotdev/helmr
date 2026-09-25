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
| `retry_delayed` | Waiting for retry backoff, environment cleanup, or a new parent handoff. |
| `cancel_requested` | Cancellation was accepted but has not converged. |
| `succeeded` | Finished with an output. |
| `failed` | Application execution failed. |
| `cancelled` | Cancellation reached a terminal state. |
| `expired` | Queued TTL or the maximum active execution duration elapsed. |
| `system_failed` | Helmr could not safely continue execution. |

A Run is pinned to the Deployment chosen at start. Promoting newer code does
not rewrite the existing Run's authority. Its Workspace is a separate durable
resource and can survive the Run.

A Task begins with Attempt 1. A retry creates the next Attempt before it is
dispatched; preparation or lease redelivery does not itself create a new Attempt. Log and event records include attempt provenance so repeated
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

If an executing environment is lost, Helmr fences its execution authority,
invalidates old memory continuations, and retains the last committed Computer disk
version. A recorded failure does not by itself prove physical cleanup is complete.

A Task with retry budget remaining starts a new Attempt from the beginning, using
the same Run, input and Deployment. Helmr waits for the old Computer execution to
be physically excluded before admitting the replacement. Infrastructure loss
consumes the same attempt budget as application failure; active execution time
accumulates across attempts. Cancellation, exhausted limits and invalid input do
not authorize another attempt. Environment preparation has a separate bounded
retry policy.

A cold retry uses the last committed disk version. Unsaved file changes can be
lost, and external side effects may already have happened. Use application-level
idempotency for external operations; Task retry does not make them exactly once.

For owned children sharing a Computer, the highest live owner restarts first.
A retryable child remains pending until the new parent Attempt reaches the same
keyed call and produces a new handoff checkpoint. The child keeps its own attempt
budget and backoff. Children on separate Computers can keep running while their
parent retries. Final parent completion or cancellation terminates its remaining
owned children, including calls skipped by a later Attempt.

An Actor does not replay an interrupted Turn automatically. Its Session retains
committed results and queued input while Helmr reconciles the environment. Future
input can start fresh execution after cleanup; the lost memory continuation is
not resumed.
