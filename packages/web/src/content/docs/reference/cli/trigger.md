---
title: helmr trigger
description: Inspect source-declared Triggers.
sidebarLabel: trigger
---

# `helmr trigger`

```text
helmr trigger list [--agent AGENT] [--cursor C] [--limit N] [--json | --jsonl]
helmr trigger get TRIGGER [--json]
```

Both commands accept project/environment scope. List defaults to 50 and allows
at most 100 records. Trigger management is intentionally read-only: edit the
source declaration and promote a new Deployment to change timing or lifecycle.

`--agent` selects the identifier declared in code, independently of current Deployment membership. Keep the same filter when continuing a cursor.
