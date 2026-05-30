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
| `task_complete` | Guest task completed with an exit code. |
| `run_failed` | Run failed before success. |
| `run_timeout` | Active run duration exceeded its limit. |
| `run_cancelled` | Run was cancelled. |

Raw protocol events from guests include stdout chunks, stderr chunks, log entries, task completion, wait requests, custom emitted events, and task output JSON.

Use event streams for live UI or agents watching progress. Use log snapshots when only the latest stdout/stderr text is needed.
