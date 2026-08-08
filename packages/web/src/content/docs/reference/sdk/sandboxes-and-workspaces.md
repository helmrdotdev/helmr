---
title: Sandboxes and Workspaces
description: Declare Sandbox capacity and operate durable Workspaces.
---

# Sandboxes and Workspaces

A Sandbox is a deployed source declaration; a Workspace is a durable resource
created from it.

```ts
export const repo = sandbox({ id: "repo" })
  .image(image("repo").from("node:24-bookworm-slim"))
  .resources({ cpu: 2, memory: "4GiB" })
```

The builder requires `.image(imageBuilder).resources({ cpu, memory })`. Memory is expressed
as `${bigint}MiB` or `${bigint}GiB`. Create externally with
`client.sandboxes.createWorkspace(declaredId, { key?, idempotencyKey?,
secrets? })`; the result is a `WorkspaceRef`.

`client.workspaces.ref(id)` exposes:

| API | Result |
| --- | --- |
| `retrieve()` | Current Workspace record and status. |
| `files.read(path)` | File bytes. |
| `files.stat(path)` | One committed file or directory entry. |
| `files.list(path, { cursor?, limit? })` | A finite page, limit 1–100. |
| `exec({ command, idempotencyKey, cwd?, env?, stdin?, timeout? })` | Bounded stdout, stderr, and exit code. |
| `delete({ idempotencyKey? })` | Deletion receipt. |

Workspace statuses are `available`, `recovery_required`, and `deleting`.
Secrets are placed at creation through `secrets.fromName(name)`
addresses; plaintext secret values are not part of a Workspace request.
