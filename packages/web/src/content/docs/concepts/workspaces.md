---
title: Workspaces
description: Durable workspace state, committed files, and bounded exec.
section: Concepts
sidebarLabel: Workspaces
order: 150
---

# Workspaces

A Workspace is a durable filesystem and execution-state object. It is scoped
to one project and environment and is created from a deployed Workspace
declaration.

The v0 public lifecycle is deliberately narrow:

| Action | Meaning |
| --- | --- |
| Create | Create a Workspace from a deployed declaration, optionally with a stable key and Secret placements. |
| Retrieve/ref | Address it by resource ID or by declaration plus key. |
| Read files | Read, list, or stat the current committed filesystem. |
| Exec | Run one bounded command and receive its terminal stdout, stderr, and exit code. |
| Delete | Remove the Workspace from normal use. |

Task start always names an existing Workspace. The Workspace can outlive a Run
and can be reused by later Task or Actor work.

Workspace declarations expose CPU and memory. Helmr chooses the v0 ephemeral
disk default internally and retains the concrete disk allocation for placement
and capacity enforcement. Persistent Workspace state is a separate storage
contract.

```ts
import { HelmrClient } from "@helmr/sdk"
import type { repositoryWorkspace } from "./helmr-definitions"

const workspace = await client.workspaces.create<typeof repositoryWorkspace>(
  "repository-agent",
  {
    key: "repo:helmrdotdev/helmr",
    idempotencyKey: "workspace:helmrdotdev/helmr",
  },
)

const result = await workspace.exec({
  command: ["git", "status", "--short"],
  cwd: "/workspace",
  idempotencyKey: "workspace:status:1",
})

const bytes = await workspace.files.read("README.md")
```

Committed file reads do not start a VM or run a shell command. Basic exec is
write-capable and bounded by request size, output size, and timeout limits.
Process handles, PTYs, materialization controls, version management, and live
filesystem management are not v0 public resources.

Secrets are placed when the Workspace is created. Repository identifiers and
other ordinary input belong in Task payloads or exec requests; secret values do
not.
