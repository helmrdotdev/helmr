---
title: helmr computer
description: Create, inspect, execute in, and delete Computers.
sidebarLabel: computer
---

# `helmr computer`

```text
helmr computer create TEMPLATE [--key KEY] [--idempotency-key KEY] [--json]
helmr computer list [--key KEY | --cursor CURSOR] [--limit N] [--json]
helmr computer get (--id UUID | --key KEY) [--json]
helmr computer sessions (--id UUID | --key KEY) [--cursor CURSOR] [--limit N] [--json]
helmr computer commands (--id UUID | --key KEY) [--cursor CURSOR] [--limit N] [--json]
helmr computer update (--id UUID | --key KEY) --delete-after-unused DURATION_OR_NEVER [--json]
helmr computer delete (--id UUID | --key KEY) [--idempotency-key KEY] [--json]
helmr computer exec (--id UUID | --key KEY) --idempotency-key KEY -- COMMAND [ARG...]
```

All commands also accept project/environment scope.

`exec` accepts `--cwd`, repeated `--set-env NAME=VALUE`, `--stdin FILE`, and
`--timeout` (default 5m, maximum 15m). It returns a Command ID after admission. Use
[`helmr command`](/docs/reference/cli/command) to read output, wait, or cancel. The `--` separator is required before the
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


## Inspect Templates and usage

Use [`helmr template list` and `get`](/docs/reference/cli/template) to inspect
available Templates. Computer `get` reports the actual instance and its origin
`template` and `deployment_id`.

`sessions` and `commands` list work currently occupying the Computer, including
terminated work whose process cleanup is pending (`unreconciled`). These are not
historical lists. Each entry includes `id`, `usage_state` and `created_at`;
usage state is distinct from the Session or Command's lifecycle status. Each
collection has its own cursor; do not move a cursor between them.

`list`, `sessions` and `commands` accept page sizes 1–100 (default 50).
Computer `list --key KEY` is an exact lookup and rejects cursor/limit.

`update` changes only retention after the Computer becomes unused. For example:

```sh
helmr computer update --key repository --delete-after-unused 1h
helmr computer update --key repository --delete-after-unused never
```
