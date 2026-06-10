---
title: Run events
description: Run event records and SDK event types.
section: Reference
sidebarLabel: Run events
order: 950
---

# Run events

Run event records are ordered by numeric cursor and exposed through the REST API, CLI, and SDK. The CLI uses the same durable event stream for `helmr events --follow` and `helmr wait`.

Each raw event record includes `run_id`. Events that originate from a worker session also include `session_id` and `attempt_number`. Run-level events that happen before a worker lease, such as queued expiry, can omit session and attempt metadata.

SDK event types:

| Type | Meaning |
| --- | --- |
| `log` | stdout/stderr bytes were observed. The event is a lightweight notification, not the log body. |
| `waitpoint_request` | A task requested a waitpoint. |
| `waitpoint_resolved` | A waitpoint was resolved. |
| `emit` | Task code emitted a custom event. |
| `task_result` | Guest task completed with an exit code. |
| `run_failed` | Run failed before success, including non-zero task exits and active duration limits. |
| `run_cancelled` | Run was cancelled. |
| `run_expired` | Queued run TTL expired before a worker started it. |

Raw protocol events include log notifications, task completion, wait requests, custom emitted events, task output JSON, and platform execution lifecycle events such as `run.execution_lost` when a worker lease expires and the attempt is no longer accepted.

Use event streams for live UI, agents watching progress, and waiting for terminal run state. `helmr wait` follows the stream, resumes with the last cursor after reconnects, and fetches the final run snapshot after a terminal event. Use run logs for stdout/stderr bytes. `helmr logs --follow` follows the dedicated log stream with a run-wide cursor, so stdout and stderr chunks can be resumed with a single `Last-Event-ID` value.
