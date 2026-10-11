---
title: helmr computer
description: Create, inspect, execute in, and delete Computers.
sidebarLabel: computer
---

# `helmr computer`

```text
helmr computer definition list [--deployment DEPLOYMENT_ID] [--limit N] [--cursor CURSOR] [--json]
helmr computer definition get DEFINITION_ID [--deployment DEPLOYMENT_ID] [--json]
helmr computer create DECLARED_ID [--key KEY] [--idempotency-key KEY] [--json]
helmr computer get (--id UUID | --key KEY) [--json]
helmr computer delete (--id UUID | --key KEY) [--idempotency-key KEY] [--json]
helmr computer exec (--id UUID | --key KEY) --idempotency-key KEY -- COMMAND [ARG...]
```

All commands also accept project/environment scope.

`exec` accepts `--cwd`, repeated `--set-env NAME=VALUE`, `--stdin FILE`, and
`--timeout` (default 5m, maximum 15m). It returns bounded stdout/stderr and exits
with the remote process exit code. The `--` separator is required before the
remote command.

## Delete a Computer

Close its Sessions and wait for commands and saves to settle before deleting a
Computer. `delete` acknowledges the request; `get` reports `deleting` while
execution stops and retained disk and checkpoint data are released. Once that
cleanup completes, the status becomes `deleted` and its reserved storage is
available for new Computers in the Environment. Deletion does not increase the
Environment's storage limit or delete Session history.

Unreferenced stored objects are removed asynchronously. Images and data still
used by other Computers remain protected.

## Secret bindings at creation

Use one JSON binding array with `--secrets-file`; it contains stable Secret IDs and placements,
never Secret values. JSON and SDK bindings use `secretId` and `allowedOrigins`.

Replace these example IDs with Secret IDs from the selected Environment.

```json
[
  {"secretId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38","env":{"name":"GH_TOKEN","mode":"protected","allowedOrigins":["https://api.github.com"]}},
  {"secretId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39","env":{"name":"PGPASSWORD","mode":"raw"}},
  {"secretId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc40","file":{"path":"/run/secrets/client.key"}}
]
```

```sh
helmr computer create reviewer --secrets-file bindings.json --idempotency-key create-reviewer
```

Bindings are fixed at Computer creation. See [Secrets](/docs/concepts/secrets)
for client support, upstream trust, rotation, and revocation limits. The Console
Computer detail page shows modes and origins without values.


## Inspect definitions

`definition list` and `definition get` inspect the current Deployment, or the
explicit `--deployment` snapshot. A list returns the pinned Deployment ID and a
continuation cursor when needed; its default limit is 50, maximum 100. These are
authored Computer definitions. `get COMPUTER_ID` inspects an actual Computer.
