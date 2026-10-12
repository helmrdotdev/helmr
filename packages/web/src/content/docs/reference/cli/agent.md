---
title: helmr agent
description: Inspect deployed Agent definitions.
sidebarLabel: agent
---

# `helmr agent`

```text
helmr agent definition list [--deployment DEPLOYMENT_ID] [--limit N] [--cursor CURSOR] [--json]
helmr agent definition get DEFINITION_ID [--deployment DEPLOYMENT_ID] [--json]
```

Every command also accepts `-p, --project` and `-e, --env`. They are required
with a saved login and rejected with an API key. See
[Credential selection](/docs/reference/cli/login#credential-selection).

## `definition list` and `definition get`

| Flag | Description |
| --- | --- |
| `--deployment DEPLOYMENT_ID` | Read this Deployment. Defaults to the current Deployment. |
| `--limit N` | Page size, 1–100. Default 50. |
| `--cursor CURSOR` | Continuation cursor from the previous page. The cursor keeps the Deployment it started with. |
| `--json` | Print one JSON page or definition. |

Each definition has its definition `id` and an `agent_id`, the Agent's stable
UUID across Deployments. Use the authored definition ID when creating a Session. See
[`helmr session create`](/docs/reference/cli/session).
