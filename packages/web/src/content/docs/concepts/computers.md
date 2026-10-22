---
title: Computers
description: Linux working environments with a retained disk, defined in source and prepared before first use.
---

# Computers

A Computer is the Linux environment an Agent's Sessions run in. It has its own
disk, processes and Secret bindings, and it outlives the Sessions that use it.
Every Computer is created from a Template definition that you declare in source
with `template()` and deploy with your Agents.

## Defining a Computer

```ts
import { agent, template, image } from "@helmr/sdk"

// Create these Secret IDs in each target Environment.
const GITHUB_TOKEN = "GITHUB_TOKEN"
const REGISTRY_TOKEN_ID = "REGISTRY_TOKEN"

export const repo = template({
  id: "repo",
  image: image("repo")
    .from("node:24-bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y git ripgrep"])
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  async prepare(build) {
    await build.exec(["git", "clone", "https://github.com/acme/app.git", "/workspace/app"])
    await build.exec("npm ci", { cwd: "/workspace/app" })
  },
  refresh: { every: "1d", maxAge: "7d" },
  buildSecrets: [{ secret: REGISTRY_TOKEN_ID, env: { name: "REGISTRY_TOKEN", mode: "raw" } }],
  secrets: [{ secret: GITHUB_TOKEN, env: { name: "GITHUB_TOKEN", mode: "raw" } }],
  deleteAfterUnused: "30d",
})

export const fixer = agent({
  id: "fixer",
  template: repo,
  async turn(turn) {
    return { received: turn.input.length }
  },
})
```

The full field list is in the [Computer reference](/docs/reference/sdk/computer).

Durations are positive integer milliseconds or a string such as `"30m"`, `"12h"`
or `"7d"`.

The disk is 32 GiB in this release. You can omit `disk`; a Computer whose
definition sets any other size cannot be created.

## The prepared image

A template's `image()` is the recipe; an image is the disk Helmr builds from
that recipe and, when you declare one, the result of running `prepare`.
Every Computer starts from a prepared image. New Computers start from a copy of
it, so work that every Computer needs (cloning a repository, installing
dependencies, warming caches) runs once instead of at the start of every
Session.

`prepare(build)` runs inside a Computer built from the `image()` recipe.
`build.exec` runs one command and rejects if it exits with a nonzero status.
Pass a string to run it through `/bin/sh -c`, or an argv array to run it
directly, with optional `cwd` and `env`. `build.signal` aborts when preparation
is cancelled. When `prepare` returns and its commands finish, Helmr saves the
disk as the prepared image. If `prepare` throws or a command fails, no image is
produced and Computers waiting for it report an `unavailable` residency with the
error code `computer_preparation_failed`.

Helmr prepares an image when a Computer needs one and no usable image exists.
Computers created at the same time share one preparation. A prepared image
belongs to one Template definition in one deployed build; deploying changed code
or definitions produces a new image the next time a Computer needs one.

## Refresh

A prepared image ages. `refresh` controls how old it may be:

- `maxAge`: a new Computer does not start from an image older than this.
  Helmr prepares a new image instead.
- `every`: the intended interval between background re-preparations.

Refresh affects only Computers created afterward. A Computer keeps the image it
started from for its whole life, and its disk then changes only through its own
work.

`maxAge` is enforced when a Computer is created. Promoting a Deployment activates
its `every` policy for background preparation. The interval is best effort: a
usable build already in progress satisfies the interval, missed intervals are
coalesced, and capacity or an executor awaiting physical shutdown can delay work.
It does not guarantee a successful image at each interval.

Helmr keeps the newest usable prepared image for each immutable recipe and images
still needed by existing Computers or waiting preparations. Superseded payloads
are reclaimed after the producing executor has stopped; image identities and
build history remain. A new preparation may produce different bytes if external
inputs changed. Historical generated image versions are not selectable through
the API.

## Build and runtime Secrets

A Template definition has two separate sets of [Secret](/docs/concepts/secrets)
bindings:

| Binding | Available to | Effect of rotation |
| --- | --- | --- |
| `buildSecrets` | `prepare` only. | An image prepared with an older Secret version is not used for new Computers; Helmr prepares a new one. Existing Computers are unaffected. |
| `secrets` | Processes in Computers created from the definition. | Follows the Secret's own delivery rules. See [Secrets](/docs/concepts/secrets). |

Anything `prepare` writes to disk becomes part of the prepared image and is copied
into every new Computer. Do not write build Secret values to files.

Bindings use stable Secret IDs, never values. Runtime bindings are fixed when a
Computer is created: it takes the definition's `secrets`, or the bindings passed
explicitly to `client.computers.create`.

## Lifecycle

Starting an Agent without an existing Computer creates a new one from the
Agent's Template definition. You can also create a Computer directly and place
Sessions on it:

```ts
const computer = await client.computers.create({
  template: "repo",
  key: "repo:acme/app",
  idempotencyKey: "create:repo:acme/app",
})

await client.sessions.create({ agent: "fixer", input: [{ type: "text", text: "Fix the failing test" }],
  computer: { id: computer.id }, })
```

`key` is an optional immutable lookup value; it does not make creation
get-or-create. Several Sessions can use one Computer and see each other's
committed files. Use separate Computers when work must not share files or
Secret placements. Workloads in one Environment are mutually trusted.

A Computer's `status` is its resource lifecycle: `available`, `deleting` or
`deleted`. Its `residency` describes where it is running; the Console shows the
residency as a Computer's status, and the lifecycle only while it is `deleting`
or `deleted`:

| Residency | Meaning |
| --- | --- |
| `starting` | Its image is being prepared or a machine is being started for it. |
| `running` | It is running on a worker. |
| `cold` | It is not running and has no ready memory checkpoint. |
| `parked` | Its durable memory checkpoint is ready; background progress is paused. |
| `restoring` | It is resuming from hibernation. |
| `parking` | Capture or physical release is in progress. |
| `unavailable` | It cannot run; `error` carries a code such as `computer_preparation_failed` or `computer_recovery_required`. |

Use `await computer.hibernate()` to request hibernation of the whole Computer,
or `await computer.wake()` to resume background execution without sending a
prompt. Both return an accepted-operation receipt; observe `computer.get()` and
`executionControl` for completion or failure. An optional `idempotencyKey` makes
retries return the same accepted operation. Hibernate rejects active foreground
work; it does not cancel work or queue a deferred sleep instruction.

Console provides the same actions and shows the affected Sessions before
hibernation. A stale confirmation is rejected if that scope changes. Wake does
not release Session holds, reopen closed Sessions or change Slack mute.

Starting a Session or a Command on a Computer starts or restores it as needed.
This applies after explicit and automatic hibernation. Rejected wake admission
preserves the sleeping checkpoint. Failures after an accepted wake require
observing the operation state; they do not imply rollback.
See [Durability](/docs/concepts/durability) for what is saved and when.

A Computer is unused when all its Sessions are finished, its Commands have
settled and its disk saves are complete. After `deleteAfterUnused`, Helmr deletes
it. New work resets that interval. Change the policy on an existing Computer
with `client.computers.ref(id).update({ deleteAfterUnused })`, or delete it
explicitly.

## Commands

A Command runs one argv command inside a Computer, independent of any Session.
It has its own ID, outcome and output logs, and it can change the Computer's
files. Commands default to `/workspace` as their working directory.
A Command owns a bounded process scope: when its main process exits, including
a successful exit, Helmr terminates remaining descendants. Starting a background
server with `sh -c 'server &'` therefore does not keep that server running after
the Command finishes.

```sh
helmr computer exec --id "$COMPUTER_ID" --idempotency-key status:1 -- git status --short
```

Inside Agent code, run local work as ordinary child processes. See
[Run commands](/docs/guides/run-commands).

## Related

- [Prepare a Computer](/docs/guides/prepare-a-computer)
- [Run commands](/docs/guides/run-commands)
- [Secrets](/docs/concepts/secrets)
- [Durability](/docs/concepts/durability)
- [Computer reference](/docs/reference/sdk/computer) and
  [CLI reference](/docs/reference/cli/computer)

A Computer can also report an `execution_wait` reason for accepted runnable work:
`scheduling` while compatible placement is pending, `environment_limit` while the
Environment's execution limits prevent admission, or `saving` while a save or
checkpoint transition blocks follow-up work. The SDK exposes this as
`executionWait`. An idle Computer reports `null`; this field does not give an ETA
or replace residency, errors, or Session holds.
