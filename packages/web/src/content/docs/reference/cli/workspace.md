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

## Secret bindings at creation

Use one JSON binding array with `--secrets-file`; it contains names and placements,
never Secret values. The wire schema uses `allowed_origins` (SDK: `allowedOrigins`).

```json
[
  {"secret":"github-token","env":{"name":"GH_TOKEN","mode":"protected","allowed_origins":["https://api.github.com"]}},
  {"secret":"database-password","env":{"name":"PGPASSWORD","mode":"raw"}},
  {"secret":"client-key","file":{"path":"/run/secrets/client.key"}}
]
```

```sh
helmr workspace create reviewer --secrets-file bindings.json --idempotency-key create-reviewer
```

Bindings are fixed at Workspace creation. See [Secrets](/docs/concepts/secrets/)
for client support, upstream trust, rotation, and revocation limits. Console
Workspaces → Create Workspace offers the same choices;
Workspace detail shows modes and origins without values.
