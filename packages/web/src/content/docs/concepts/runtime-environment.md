---
title: Runtime environment
description: What Helmr provides inside a running task sandbox.
section: Concepts
sidebarLabel: Runtime environment
order: 145
---

# Runtime Environment

Helmr runs task code in a Firecracker-backed Linux sandbox. You define the task
image and task logic; Helmr manages the substrate around it so normal Linux
tools, package managers, CLIs, and agent SDKs can run predictably.

## What You Define

Task authors define:

| Surface | Controlled by |
| --- | --- |
| Image contents | `image(...).from(...).run(...).copy(...)` |
| Runtime dependencies | Package manager and install steps in the image build |
| Workspace path | `sandbox(...).workspace("/path")` |
| CPU and memory | `sandbox(...).resources(...)` |
| Secrets | Task `secrets` declarations and run-time bindings |
| Task behavior | The exported `task(...).run` function |

TypeScript task images must provide Node.js 22.18 or newer as `node` on `PATH`.
Helmr injects its adapter protocol, but task code runs with the Node runtime,
tools, package manager, and dependencies installed in your image.

## What Helmr Provides

Helmr manages:

- Firecracker VM lifecycle, guest boot, and guest agent startup.
- The checked-out GitHub workspace.
- Deployment task source used to load the task module.
- Secret materialization as environment variables, files, or directories.
- Runtime filesystems such as `/proc`, `/dev`, `/dev/pts`, `/dev/shm`, `/tmp`,
  and `/run`.
- Basic network readiness, DNS resolver files, and hostname setup.
- Logs, events, waitpoints, timeouts, and run status.
- Checkpoint and restore compatibility checks.

These details are product-managed. Task code should rely on the resulting Linux
behavior, not on Helmr's internal paths, guest init scripts, Firecracker devices,
vsock ports, or host networking implementation.

## Filesystem Behavior

Tasks start in the checked-out workspace directory. Use relative paths for
workspace files. Absolute paths behave like normal Linux paths inside the
sandbox image.

Helmr prepares the sandbox image root immediately before execution. It may add
or replace product-managed runtime files inside the sandbox root, including
adapter files and standard runtime identity files. These changes are part of the
run environment and are not persisted back into your image definition.

The runtime provides:

- `/proc` for process and system information.
- `/dev` with common character devices.
- `/dev/pts` and `/dev/ptmx` for tools that need pseudo-terminals.
- `/dev/shm` for shared memory.
- `/tmp` for temporary files.
- The configured workspace mount path.

Avoid using reserved runtime paths such as `/dev`, `/proc`, `/sys`, `/run`,
`/tmp`, and `/opt/helmr` as custom workspace or application roots.

## Network And DNS

Basic networking is product-managed. A task sandbox should be able to use
ordinary DNS and outbound network clients without configuring resolver files.

Inside the sandbox image, Helmr provides:

- A default route when worker networking is available.
- `/etc/resolv.conf` with runtime DNS servers.
- `/etc/hostname` containing `helmr-sandbox`.
- `/etc/hosts` with localhost and `helmr-sandbox` entries.

Do not depend on `/run/resolv.conf`, tap devices, CNI names, nftables rules, or
other implementation details. If Helmr exposes user-facing egress controls, they
should be configured through documented product policy rather than by mutating
resolver files or host networking state from task code.

Self-hosted workers still need outbound access to the services your tasks use,
as well as GitHub, S3, ECR, AWS APIs, and the Helmr control URL.

## Secrets

Payload is audit data and is stored in plaintext. Put credentials in declared
secrets, not in payload.

Secrets are injected only for runs that bind them. File and directory secrets
are materialized inside the sandbox or workspace according to the declared
placement, permissions, and owner. If the runtime user cannot read or traverse a
secret path, the run fails instead of silently weakening permissions.

## Failure Boundaries

Helmr separates runtime failures by layer:

| Failure area | Typical meaning |
| --- | --- |
| Deployment build | Task project parse, compile, package metadata, or dependency problem |
| Image preparation | Missing `node`, missing installed task dependencies, unsafe path, or image root issue |
| Guest runtime | Firecracker boot, guest agent health, filesystem mount, or network readiness issue |
| Task process | Your task code exited non-zero or threw an error |
| Timeout | The run exceeded its configured duration |
| Waitpoint | A waitpoint timed out or failed to resume |

When a run fails before task code starts, inspect run events and worker logs
before treating it as a task bug.
