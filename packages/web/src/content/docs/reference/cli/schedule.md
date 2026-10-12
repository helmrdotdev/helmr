---
title: helmr schedule
description: Inspect source-declared Schedules.
sidebarLabel: schedule
---

# `helmr schedule`

```text
helmr schedule list [--agent AGENT | --agent-id UUID] [--cursor C] [--limit N] [--json | --jsonl]
helmr schedule get SCHEDULE [--json]
```

Both commands accept project/environment scope. List defaults to 50 and allows
at most 100 records. Schedule management is intentionally read-only: edit the
source declaration and promote a new Deployment to change timing or lifecycle.

`--agent` selects the identifier declared in code, independently of current Deployment membership. `--agent-id` selects the stable UUID. They are mutually exclusive. Keep the same filter when continuing a cursor.
