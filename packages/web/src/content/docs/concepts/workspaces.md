---
title: Workspaces
description: Durable workspace behavior, live materialization, exec, and PTY.
section: Concepts
sidebarLabel: Workspaces
order: 150
---

# Workspaces

A workspace is a durable filesystem and work-state object. It is not owned by a
single run or session.

Sessions attach to workspaces. Direct workspace operations also attach to
workspaces without creating sessions. This lets a task finish while the
workspace remains available for later inspection, another task invocation, or a
direct command or terminal operation.

## Lifecycle

Workspaces are scoped to a project and environment. Creating a workspace pins it
to a deployed sandbox definition, so later execution uses the same sandbox
contract.

The current user-facing lifecycle is:

| Action | Meaning |
| --- | --- |
| Create | Create a durable workspace from a deployed sandbox. |
| Retrieve/list | Read workspace metadata and lifecycle state. |
| Update | Replace user metadata and tags. |
| Read files | Read, list, or stat files from persisted ready versions. |
| Materialize | Ask Helmr to prepare a live VM/worker materialization for the workspace. |
| Connect | Return the current live materialization, creating or requesting one when needed. |
| Stop | Request controlled shutdown of the live materialization. |
| Delete/archive | Remove the workspace from normal use. |

## Materialization

A materialization is the live execution instance for a workspace. It is the
bridge between durable workspace state and active execution.

Task runs, workspace execs, and PTY sessions use materializations when they need
to execute code. If a workspace is not live, Helmr can request a materialization
before dispatching the operation.

## Files and Versions

Workspace file reads inspect persisted read-only version artifacts. They do not
start a VM, request materialization, acquire a workspace lease, or run shell
commands inside the workspace.

```ts
import { workspaces } from "@helmr/sdk"

const workspace = workspaces.open("workspace-id")
const bytes = await workspace.files.read("src/app.ts")
const entries = await workspace.files.list("src")
const stat = await workspace.files.stat("src/app.ts")
```

By default, file reads use `source: "current"` and read the workspace's ready
`currentVersionId`. To inspect a specific ready version from the same
workspace, pass `{ source: "version", versionId }`. `source: "live"` is reserved
for future live file access and is not implemented.

```ts
const version = await workspace.versions.retrieve("version-id")
const versionBytes = await workspace.files.read("src/app.ts", {
  source: "version",
  versionId: version.id,
})
```

## Exec

Workspace exec creates a durable command handle on a workspace:

```ts
import { workspaces } from "@helmr/sdk"

const workspace = workspaces.open("workspace-id")
const exec = await workspace.exec(["bash", "-lc", "echo ok"], {
  cwd: "/workspace",
})
```

The handle records command state, exit information, stdin cursor, stdout cursor,
and stderr cursor. Stdout and stderr are durable stream chunks. Blocking helper
methods can wait for terminal state, but the handle remains the source of truth.

Execs are write-capable in the current API. Read-only workspace exec is not part
of the current documented contract.

## PTY

Workspace PTY creates a durable interactive terminal handle:

```ts
const pty = await workspaces.open("workspace-id").pty.create({
  cwd: "/workspace",
  cols: 100,
  rows: 32,
})
```

The PTY handle records open/resize/close state and input/output cursors. Output
can be read as durable chunks or followed over SSE.

## Streams

Exec stdout/stderr and PTY output are cursor-based durable streams. Read APIs
return chunks after a cursor. Follow APIs use server-sent events and reconnect
from the last cursor.

Stream cursors are byte offsets, not page numbers. If a cursor is older than the
retained stream window, the API returns a cursor-expired error with the earliest
available cursor.

## Runtime Directory

Tasks and direct workspace operations run inside the configured sandbox
workspace mount path, usually `/workspace`. Use relative paths for workspace
files. Absolute paths behave like normal Linux paths inside the sandbox image.

The workspace mount path is part of the sandbox definition. It must be absolute,
cannot be `/`, cannot contain `..`, and cannot overlap Helmr-managed runtime
paths such as `/dev`, `/proc`, `/sys`, `/run`, `/tmp`, or `/opt/helmr`.

## Repository Access

Repository access is a task-level or command-level integration. Pass repository
identifiers in payload and credentials through declared secrets or direct exec
environment values. Workspace creation stays provider-neutral; tasks and
commands decide which external systems to fetch from.
