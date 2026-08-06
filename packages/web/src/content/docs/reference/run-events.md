---
title: Run events
description: Run event records and SDK event types.
section: Reference
sidebarLabel: Run events
order: 950
---

# Run events

Run event records are ordered by an opaque cursor exposed through the REST API,
CLI, and SDK. Each request returns one finite page.

Each raw event record includes `run_id`. Events that originate from worker execution can also include `attempt_number`. Run-level events that happen before a worker starts, such as queued expiry, can omit attempt metadata.

SDK event types:

| Type | Meaning |
| --- | --- |
| `log` | stdout/stderr bytes were observed. The event is a lightweight notification, not the log body. |
| `actor_input_wait` / `token_wait` / `timer_wait` | A Run parked on Actor input, Token completion, or time. |
| wait completion or timeout | A parked Wait reached a terminal condition. |
| `task_result` | Guest task completed with an exit code. |
| `run_failed` | Run failed before success, including non-zero task exits and active duration limits. |
| `run_cancelled` | Run was cancelled. |
| `run_expired` | Queued run TTL expired before a worker started it. |

Raw protocol events include log notifications, Task completion, waits, Task
result JSON, Run metadata updates, and platform execution lifecycle events such
as `run.execution_lost` when a worker Lease expires. Events are observation,
not application-output authority: durable long-lived output is read from Actor
records, while a one-shot Task returns its terminal result.

Use Run resources as terminal-status authority. `helmr run wait` and
`helmr run logs --follow` poll finite pages and Run state from the last opaque
cursor. Run events are observation, not application-output authority.
