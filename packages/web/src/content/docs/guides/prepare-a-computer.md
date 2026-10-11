---
title: Prepare a Computer
description: Build a Computer's image, prepare its starting disk with prepare(), and roll out changes.
---

# Prepare a Computer

A Template definition controls what every new Computer starts with: the Linux
image, and optionally a `prepare` step that sets up the disk once so that
Sessions do not repeat the work. This guide builds both and rolls out a change.
For how prepared baselines, refresh and Secret bindings behave, see
[Computers](/docs/concepts/computers).

The examples use the `demo` Project's `staging` Environment, with the CLI
configured (see [Quickstart](/docs/quickstart)).

## Build the image

Compose the image in TypeScript with `image()` and attach it to a definition:

```ts
import { template, image, source } from "@helmr/sdk"

const repoImage = image("repo")
  .from("node:24-bookworm-slim")
  .run(["sh", "-ceu", "apt-get update && apt-get install -y git ripgrep"])
  .copy(source.file("bin/review-helper"), "/usr/local/bin/review-helper")
  .run(["chmod", "+x", "/usr/local/bin/review-helper"])
  .env("GIT_TERMINAL_PROMPT", "0")
  .workdir("/workspace")

export const repo = template({
  id: "repo",
  image: repoImage,
  resources: { cpu: 2, memory: "4GiB" },
})
```

| Step | Effect |
| --- | --- |
| `from(ref)` | Base image. |
| `run(argv)` | Run a command. Arguments are an argv array; call a shell explicitly, as above, when you need shell syntax. |
| `copy(source, destination)` | Copy a project file (`source.file(path)`) or directory (`source.directory(path)`) to an absolute path in the image. A file lands at the destination, or inside it when the destination is an existing directory; a directory's contents are merged into the destination. |
| `copyFrom(destination, otherImage, sourcePath)` | Copy a path out of another `image()`. |
| `workdir(path)`, `env(key, value)`, `user(name)` | Set the working directory, an environment variable or the user. |

`helmr deploy` builds the image in Helmr's builder on your machine, as part of
the Deployment. The image supplies the operating system, libraries and tools
your Agent runs; Helmr supplies the Node.js runtime, your compiled Agent code and
its installed dependencies separately, so do not install those into the image.
The environment that installs your project's dependencies at deploy time is
configured separately with `build.builder` in
[`helmr.config.ts`](/docs/reference/config).

Do not put credentials in image steps. Use Secrets, as below.

## Prepare the disk

Add `prepare` for work that should happen once and be copied into every new
Computer, such as cloning a repository or installing its dependencies:

```ts
export const repo = template({
  id: "repo",
  image: repoImage,
  resources: { cpu: 2, memory: "4GiB" },
  async prepare(build) {
    await build.exec(["git", "clone", "--depth=1", "https://github.com/acme/app.git", "/workspace/app"])
    await build.exec("npm ci", { cwd: "/workspace/app" })
  },
})
```

`build.exec` takes an argv array, or a string that runs through `/bin/sh -c`,
with optional `cwd` and `env`. It resolves when the command exits with status 0
and rejects otherwise. `build.signal` is aborted if preparation is cancelled.

`prepare` runs on a worker, inside a Computer built from the image, the first
time a Computer from this definition needs a baseline. When it returns and its
commands finish, the disk is saved, and new Computers start from a copy. If it
throws, the waiting Computer reports `residency: "unavailable"` with an error:

```sh
helmr computer get --id COMPUTER_ID --project demo --env staging --json
```

## Use Secrets while preparing

`buildSecrets` binds Secrets for the `prepare` step only. Create the Secret in the
Environment you deploy to and use its ID; see
[Manage secrets](/docs/guides/manage-secrets).

```ts
// ID from `helmr secret create REGISTRY_TOKEN --json` in this Environment.
const REGISTRY_TOKEN_ID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"

export const repo = template({
  id: "repo",
  image: repoImage,
  resources: { cpu: 2, memory: "4GiB" },
  buildSecrets: [{ secretId: REGISTRY_TOKEN_ID, env: { name: "REGISTRY_TOKEN", mode: "raw" } }],
  async prepare(build) {
    await build.exec(["git", "clone", "--depth=1", "https://github.com/acme/app.git", "/workspace/app"])
    await build.exec("npm ci", { cwd: "/workspace/app" })
  },
})
```

The deploy is rejected if a referenced Secret ID does not belong to the target
Environment. Everything `prepare` writes is copied into every new Computer, so
never write a build Secret's value to a file. Bindings that Computers need while
they run go in `secrets`, not `buildSecrets`.

## Limit the baseline's age

```ts
refresh: { every: "1d", maxAge: "7d" },
```

With `maxAge`, a new Computer does not start from a baseline older than that;
Helmr prepares a fresh one first. Both fields are durations, and `every` is
required. In this release Helmr does not re-prepare baselines in the background
on the `every` interval; see [Computers](/docs/concepts/computers).

## Roll out a change

1. Edit the image, `prepare`, `buildSecrets` or `refresh`.
2. Deploy:

   ```sh
   helmr deploy . --project demo --env staging
   ```

3. Confirm the definition in the new current Deployment:

   ```sh
   helmr template definition get repo --project demo --env staging --json
   ```

Computers created after the Deployment is promoted use the new definition; the
first one prepares a new baseline. Existing Computers keep the disk they have,
and existing Sessions stay on the Deployment they started with. To use the change,
start new Sessions without `--computer`, or create a new Computer as described
in [Run commands](/docs/guides/run-commands).

Rotating a Secret named in `buildSecrets` also makes the next new Computer
prepare a fresh baseline with the new value. Existing Computers are not changed.
