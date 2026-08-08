---
title: Run events
description: The checked-in public Run event record and terminal event kinds.
sidebarLabel: Run events
---

# Run events

`GET /v1/runs/{runID}/events`, `client.runs.events()`, and `helmr run events`
expose finite pages ordered by an opaque cursor. The public SDK record is:

```ts
interface RunEventRecord {
  id: string
  runId: string
  attemptNumber?: number
  category: string
  severity: "debug" | "info" | "warn" | "error"
  source: string
  kind: string
  message: string
  attributes: JsonValue
  occurredAt: string
  at: string
}
```

The REST wire uses snake case (`run_id`, `attempt_number`, `occurred_at`). Its
page shape is `{ "events": [...], "next_cursor"?: string }`. An event that is
not tied to an execution attempt may omit `attempt_number`.

Only these event-kind identifiers are exported as checked-in terminal
contracts:

| Kind | Terminal Run status |
| --- | --- |
| `run.completed` | `succeeded` |
| `run.failed` | `failed` or `system_failed`, according to the Run resource |
| `run.cancelled` | `cancelled` |
| `run.expired` | `expired` |

Other `category`, `source`, and `kind` strings are observational values, not a
closed public enum. Use the Run resource as status and output authority; do not
infer a complete lifecycle from event names.
