---
title: Workspaces
description: Durable filesystem state created from deployed Sandbox definitions.
---

# Workspaces

A Workspace is a durable project-and-environment resource created from a
deployed Sandbox. The Sandbox fixes its image, CPU, and memory; Workspace
creation may add an immutable key and Secret placements.

The public lifecycle is deliberately bounded:

| Operation | Contract |
| --- | --- |
| Create | Create from a Sandbox declared ID, optionally with key, Secrets, and idempotency key. |
| Retrieve/list | Address by UUID, page the collection, or look up one exact key. |
| Exec | Run one bounded command and receive exit code, stdout, and stderr. |
| Delete | Remove the Workspace from normal use. |

Every external Task or Actor start supplies a Workspace reference. The
Workspace does not belong to that Run and can outlive it. Reusing a Workspace
lets later work observe committed files; using separate Workspaces isolates
state and Secret placement.

```ts
const workspace = await client.sandboxes.createWorkspace(
  "repository-agent",
  {
    key: "repo:helmrdotdev/helmr",
    idempotencyKey: "workspace:helmrdotdev/helmr",
  },
)

const result = await workspace.exec({
  command: ["git", "status", "--short"],
  cwd: "/workspace",
  timeout: "5m",
  idempotencyKey: "workspace:status:1",
})
```

Direct exec is not a Run: it has one terminal response and no Task result,
Run logs, or events. It accepts explicit argv, optional cwd, environment,
stdin, timeout, and a required idempotency key.

Exec runs the supplied command inside the mounted Workspace and may mutate its
filesystem. It is a command-execution capability, not a read-only file API.

Workspace state and the image root are distinct. Use relative paths for files
that should live in the mounted Workspace. Secret values never belong in exec
arguments, environment overrides, or task payload.
