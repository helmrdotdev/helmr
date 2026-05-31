---
title: Run events
description: Run event records and SDK event types.
section: Reference
sidebarLabel: Run events
order: 950
---

# Run events

Run event records are ordered by numeric cursor and exposed through the REST API, CLI, and SDK.

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

Raw protocol events from guests include stdout chunks, stderr chunks, log entries, task completion, wait requests, custom emitted events, and task output JSON.

Use event streams for live UI or agents watching progress. Use log snapshots when only the latest stdout/stderr text is needed.
