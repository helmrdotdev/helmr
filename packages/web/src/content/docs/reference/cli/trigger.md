---
title: helmr trigger
description: Inspect source-declared Triggers.
sidebarLabel: trigger
---

# `helmr trigger`

```text
helmr trigger list [--agent AGENT] [--key KEY] [--all] [--cursor C] [--limit N] [--json | --jsonl]
helmr trigger get TRIGGER [--json]
helmr trigger occurrences ACTIVATION [--cursor C] [--json]
```

All commands accept project/environment scope. List defaults to 50 and allows
at most 100 records. Trigger management is intentionally read-only: edit the
source declaration and promote a new Deployment to change timing or lifecycle.

List defaults to current activations. `--all` includes retained historical
activations, including those for Agents absent from current or deleted.
`--agent` selects the identifier declared in code; `--key` selects the logical
trigger ID declared on that Agent. Keep the same view and filters when continuing
a cursor. `get` accepts the activation UUID, including historical activations.

`occurrences` lists recorded outcomes for one activation, with scheduled time,
disposition and optional reason. JSON also includes evaluation time and nullable
Session and Turn IDs. It requires Session viewing permission and supports a saved human login or an
Environment-bound API key. With a saved login, pass `-p PROJECT -e ENV`; with an
API key, omit those selectors. Pages contain up to 50 outcomes; use the returned
cursor to continue.
See [Trigger history](/docs/reference/api/overview#trigger-activations-and-occurrences).
