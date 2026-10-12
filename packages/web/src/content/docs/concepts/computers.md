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
  computer: repo,
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

## The prepared baseline

Every Computer starts from a prepared baseline: a disk built from the
definition's image and, when you declare one, the result of running `prepare`.
New Computers start from a copy of that baseline, so work that every Computer
needs (cloning a repository, installing dependencies, warming caches) runs once
instead of at the start of every Session.

`prepare(build)` runs inside a Computer built from the image. `build.exec`
runs one command and rejects if it exits with a nonzero status. Pass a string to
run it through `/bin/sh -c`, or an argv array to run it directly, with optional
`cwd` and `env`. `build.signal` aborts when preparation is cancelled. When
`prepare` returns and its commands finish, Helmr saves the disk as the baseline.
If `prepare` throws or a command fails, no baseline is produced and Computers
waiting for it report an `unavailable` residency with the error code
`computer_preparation_failed`.

Helmr prepares a baseline when a Computer needs one and no usable baseline
exists. Computers created at the same time share one preparation. A baseline
belongs to one Template definition in one deployed build; deploying changed
code or definitions produces a new baseline the next time a Computer needs one.

## Refresh

A prepared baseline ages. `refresh` controls how old it may be:

- `maxAge`: a new Computer does not start from a baseline older than this.
  Helmr prepares a new baseline instead.
- `every`: the intended interval between background re-preparations.

Refresh affects only Computers created afterward. A Computer keeps the baseline
it started from for its whole life, and its disk then changes only through its
own work.

In this release, `maxAge` is enforced when a Computer is created. Helmr records
`every` when a Deployment is promoted, but it does not yet re-prepare baselines
in the background on that interval; a new baseline is prepared when a Computer
needs one.

## Build and runtime Secrets

A Template definition has two separate sets of [Secret](/docs/concepts/secrets)
bindings:

| Binding | Available to | Effect of rotation |
| --- | --- | --- |
| `buildSecrets` | `prepare` only. | A baseline prepared with an older Secret version is not used for new Computers; Helmr prepares a new one. Existing Computers are unaffected. |
| `secrets` | Processes in Computers created from the definition. | Follows the Secret's own delivery rules. See [Secrets](/docs/concepts/secrets). |

Anything `prepare` writes to disk becomes part of the baseline and is copied
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
`deleted`. Its `residency` describes where it is running:

| Residency | Meaning |
| --- | --- |
| `starting` | Its baseline is being prepared or a machine is being started for it. |
| `running` | It is running on a worker. |
| `cold` | It is not running on a worker. A hibernated Computer is `cold`. |
| `restoring` | It is resuming from hibernation. |
| `parking` | Its machine is being released. |
| `unavailable` | It cannot run; `error` carries a code such as `computer_preparation_failed` or `computer_recovery_required`. |

Starting a Session or a Command on a Computer starts or restores it as needed.
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
