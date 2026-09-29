---
title: Computers
description: Durable filesystem state created from deployed Sandbox definitions.
---

# Computers

A Computer is a durable project-and-environment resource created from a
deployed Sandbox. The Sandbox fixes its image, CPU, and memory; Computer
creation may add an immutable key and Secret placements.

The public lifecycle is deliberately bounded:

| Operation | Contract |
| --- | --- |
| Create | Create from a Sandbox declared ID, optionally with key, Secrets, and idempotency key. |
| Retrieve/list | Address by UUID, page the collection, or look up one exact key. |
| Exec | Admit a Command and return a handle for its outcome and logs. |
| Delete | Remove the Computer from normal use. |

Every external Task or Actor start supplies a Computer reference. The
Computer does not belong to that Run and can outlive it. Reusing a Computer
lets later work observe committed files; using separate Computers isolates
state and Secret placement.

```ts
const computer = await client.sandboxes.createComputer(
  "repository-agent",
  {
    key: "repo:helmrdotdev/helmr",
    idempotencyKey: "computer:helmrdotdev/helmr",
  },
)

const command = await computer.exec({
  command: ["git", "status", "--short"],
  cwd: "/workspace",
  timeout: "5m",
  idempotencyKey: "computer:status:1",
})
const outcome = await command.wait()
```

The external client returns a Command handle after admission. Use `wait()` for
its terminal outcome and `logs()` or `streamLogs()` for output. Reconnect with
`client.commands.ref(command.id)`. Commands have their own identity and logs;
they do not create a Run. Exec accepts explicit argv, optional cwd, environment,
stdin, timeout, and a required idempotency key.

Inside an Actor or Task, use a local child process for CLI work on its Computer,
or a child Task for work on another Computer.

Exec runs the supplied command inside the mounted Computer and may mutate its
filesystem. It is a command-execution capability, not a read-only file API.

Computer state and the image root are distinct. The durable working directory is
`/workspace` inside the Computer. Commands default to this directory, and an
explicit Command `cwd` must be within it. Secret values never belong in exec
arguments, environment overrides, or task payload.
