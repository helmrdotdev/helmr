---
title: Run events
description: Run event records and SDK event types.
section: Reference
sidebarLabel: Run events
order: 950
---

# Run events

Run event records are ordered by numeric cursor and exposed through the REST API, CLI, and SDK. The CLI uses the same durable event stream for `helmr run events --follow` and `helmr run wait`.

Each raw event record includes `run_id`. Events that originate from a worker lease also include `run_lease_id` and `attempt_number`. Run-level events that happen before a worker lease, such as queued expiry, can omit lease and attempt metadata.

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

Raw protocol events include log notifications, task completion, waits, task output JSON, stream append notifications, run metadata update notifications, and platform execution lifecycle events such as `run.execution_lost` when a worker lease expires and the attempt is no longer accepted. Stream and metadata event payloads are notifications for timeline subscribers; read user-facing stream payloads through the session stream APIs and current run metadata from the run snapshot.

Use event streams for live UI, agents watching progress, and waiting for terminal run state. `helmr run wait` follows the stream, resumes with the last cursor after reconnects, and fetches the final run snapshot after a terminal event. Use run logs for stdout/stderr bytes. `helmr run logs --follow` follows the dedicated log stream with a run-wide cursor, so stdout and stderr chunks can be resumed with a single `Last-Event-ID` value.
