---
title: Run events
description: Run event records and SDK event types.
section: Reference
sidebarLabel: Run events
order: 950
---

# Run events

Run event records are ordered by numeric cursor and exposed through the REST API, CLI, and SDK.

Each raw event record includes `run_id`. Events that originate from a worker execution also include `execution_id` and `attempt_number`. Run-level events that happen before a worker lease, such as queued expiry, can omit execution and attempt metadata.

SDK event types:

| Type | Meaning |
| --- | --- |
| `log` | stdout/stderr bytes were observed. |
| `waitpoint_request` | A task requested a waitpoint. |
| `waitpoint_resolved` | A waitpoint was resolved. |
| `emit` | Task code emitted a custom event. |
| `task_result` | Guest task completed with an exit code. |
| `run_failed` | Run failed before success, including non-zero task exits and active duration limits. |
| `run_cancelled` | Run was cancelled. |
| `run_expired` | Queued run TTL expired before a worker started it. |

Raw protocol events include stdout chunks, stderr chunks, log entries, task completion, wait requests, custom emitted events, task output JSON, and platform execution lifecycle events such as `run.execution_lost` when a worker lease expires and the attempt is no longer accepted.

Use event streams for live UI or agents watching progress. Use log snapshots when only the latest stdout/stderr text is needed.
