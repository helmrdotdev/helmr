---
title: helmr workspace
description: Create, inspect, execute in, and delete Workspaces.
sidebarLabel: workspace
---

# `helmr workspace`

```text
helmr workspace create DECLARED_ID [--key KEY] [--idempotency-key KEY] [--json]
helmr workspace get (--id UUID | --key KEY) [--json]
helmr workspace delete (--id UUID | --key KEY) [--idempotency-key KEY] [--json]
helmr workspace exec (--id UUID | --key KEY) --idempotency-key KEY -- COMMAND [ARG...]
```

All commands also accept project/environment scope.

`exec` accepts `--cwd`, repeated `--set-env NAME=VALUE`, `--stdin FILE`, and
`--timeout` (default 5m, maximum 15m). It returns bounded stdout/stderr and exits
with the remote process exit code. The `--` separator is required before the
remote command.
