---
title: Run events
description: Run event records and SDK event types.
section: Reference
sidebarLabel: Run events
order: 950
---

# Run events

Run event records are ordered by an opaque cursor exposed through the REST API, CLI, and SDK. The CLI uses the same durable event stream for `helmr run events --follow` and `helmr run wait`.

Each raw event record includes `run_id`. Events that originate from worker execution can also include `attempt_number`. Run-level events that happen before a worker starts, such as queued expiry, can omit attempt metadata.

SDK event types:

| Type | Meaning |
| --- | --- |
| `log` | stdout/stderr bytes were observed. The event is a lightweight notification, not the log body. |
| `stream_wait` / `token_wait` / `timer_wait` | A task parked on an input stream, token, or timer. |
| `stream_wait_completed` / `token_wait_completed` / `timer_wait_completed` | A parked wait resolved. |
| `stream_wait_timed_out` / `token_wait_timed_out` / `timer_wait_timed_out` | A parked wait timed out. |
| `task_result` | Guest task completed with an exit code. |
| `run_failed` | Run failed before success, including non-zero task exits and active duration limits. |
| `run_cancelled` | Run was cancelled. |
| `run_expired` | Queued run TTL expired before a worker started it. |

Raw protocol events include log notifications, Task completion, waits, Task
result JSON, Run metadata updates, and platform execution lifecycle events such
as `run.execution_lost` when a worker Lease expires. Events are observation,
not application-output authority: durable long-lived output is read from Actor
records, while a one-shot Task returns its terminal result.

Use event streams for live UI, agents watching progress, and waiting for terminal run state. `helmr run wait` follows the stream, resumes with the last cursor after reconnects, and fetches the final run snapshot after a terminal event. Use run logs for stdout/stderr bytes. `helmr run logs --follow` follows the dedicated log stream with a run-wide cursor, so stdout and stderr chunks can be resumed with a single `Last-Event-ID` value.
