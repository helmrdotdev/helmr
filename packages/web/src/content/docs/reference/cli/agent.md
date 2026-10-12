---
title: helmr agent
description: Inspect Agents in the current or a selected Deployment.
sidebarLabel: agent
---

# `helmr agent`

```text
helmr agent list [--deployment DEPLOYMENT_ID] [--limit N] [--cursor CURSOR] [--json]
helmr agent get AGENT [--deployment DEPLOYMENT_ID] [--json]
```

`AGENT` is the `id` declared in `agent({ id: "support", ... })`. The response
calls this value `agent`. It is the only public Agent identifier.
Agent reads also include the default `template` and resolved `deployment_id`.
Use `helmr session create support` to create a Session.

Without `--deployment`, reads use the current Deployment. An explicit selector
inspects that exact Deployment, including historical content. An Agent absent
from the selected Deployment returns not found; the CLI does not choose an older
configuration. Configuration changes belong in code and a new Deployment.

| Flag | Description |
| --- | --- |
| `--deployment ID` | Inspect this Deployment instead of current. |
| `--limit N` | List page size, 1–100; default 50. |
| `--cursor CURSOR` | Continue the same Deployment snapshot. |
| `--json` | Print the resource or page as JSON. |

Commands accept `-p, --project` and `-e, --env`. They are required with a saved
login and rejected with an API key. See [Credential selection](/docs/reference/cli/login#credential-selection).
