# Helmr

Helmr is a self-hosted runtime for coding agents.

It provides the infrastructure around an agent SDK: durable writable
workspaces, controlled credentials, logs, run history, and approval points
before a task writes back. Task code is written in TypeScript and runs inside
Firecracker-backed Linux guests managed by your own control plane and workers.

## Status

Helmr is in early active development. APIs, deployment shape, and operational
defaults may change before a stable release. The current codebase is best suited
for contributors, early adopters, and self-hosted evaluation.

## What Helmr provides

- TypeScript tasks that declare images, sandboxes, resources, secrets, waitpoints,
  and run logic
- Durable writable workspaces mounted inside isolated Linux guests
- Operator waitpoints before reviews, patches, or other side effects
- Run status, logs, events, payloads, and history in the control plane
- Task-declared secrets injected only at run time
- A runtime boundary you own: your AWS account, your integrations, your workers
- A Go control plane, worker, `helmr` CLI, TypeScript SDK, and console UI

## Repository layout

- `cmd/` - Go binaries for the CLI, control plane, worker, and guest agent
- `internal/` - Go control-plane, worker, executor, database, and server code
- `sdk/typescript/` - public task authoring and runtime client APIs
- `runtime/typescript/` - guest-side TypeScript adapter
- `proto/` - shared protocol definitions and generated bindings
- `packages/console/` - self-hosted dashboard
- `packages/web/` - public website and documentation site
- `examples/` - runnable task projects
- `images/` - Firecracker guest boot image recipes
- `infra/aws/` - OpenTofu/Terraform modules and deployment examples
- `nix/` and `scripts/` - pinned development, CI, and smoke-test entrypoints

## Prerequisites

The repository is built around a Nix-pinned toolchain. Use Nix when possible so
Go, Bun, Buf, PostgreSQL, and infrastructure tooling match CI.

```sh
nix develop
nix run .#doctor
```

Without Nix, install the versions expected by `go.mod`, `package.json`, and the
CI scripts. Firecracker execution requires a Linux host with KVM; macOS is
useful for control-plane, SDK, CLI, and console development but cannot run the
Linux microVM smoke path locally.

## Quick start

Start the local control plane and console:

```sh
nix develop
make dev
```

The dev stack starts a disposable PostgreSQL database when `HELMR_DATABASE_URL`
is not set, runs the control plane, and serves the console at:

```text
http://127.0.0.1:3000/dev/login
```

Use that URL to create a local owner session and inspect seeded runs.

## Define a task

A task binds a sandbox, TypeScript run logic, declared secrets, and optional
operator waitpoints. The code inside the task can call any agent SDK or tool;
Helmr owns the adapter protocol around it.

Create a task project with `helmr.config.ts` and one or more task modules:

```ts
import { cache, image, sandbox, source, task, wait } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"
import { z } from "zod"

const payload = z.object({
  prNumber: z.number().int().positive(),
})

const base = image("repo-agent")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("repo-agent-bun") }],
  })
  .run([
    "sh",
    "-ceu",
    "apt-get update && apt-get install -y git ripgrep",
  ])

const sbx = sandbox("repo-agent")
  .image(base)
  .resources({ cpu: 2, memory: "4Gi" })

export const reviewPr = task({
  id: "review-pr",
  sandbox: sbx,
  maxDuration: 900,
  secrets: [{ name: "OPENAI_API_KEY", env: "OPENAI_API_KEY" }],
  payload,
  run: async (event, ctx) => {
    // Call your agent SDK or review tooling here.
    const summary = await reviewPullRequest({
      cwd: process.cwd(),
      prNumber: event.prNumber,
      token: process.env.OPENAI_API_KEY ?? "",
    })

    const decisionToken = await wait.createToken({ timeout: 900 })
    const decision = await wait.forToken(decisionToken, {
      schema: z.object({ approved: z.boolean() }),
      metadata: {
        summary,
        prompt: "Post this review to GitHub?",
      },
    }).unwrap()
    if (decision.approved) {
      await writeFile("review-summary.txt", `${summary}\n`)
    }
  },
})
```

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  project: "github-pr-review",
  dirs: ["./tasks"],
})
```

Tasks start in the mounted workspace directory. Use relative paths for workspace
files; absolute paths keep normal Linux container semantics.

See [examples/](examples/) for deployable task projects, including dependency
caching, CLI tooling, operator waitpoints, task secrets, and GitHub PR review
flows.

## Run A Task

Remote runs execute a deployed task in an attached writable workspace. If no
workspace is supplied, Helmr creates one from the deployed task's sandbox:

```sh
helmr deploy PATH/TO/TASK_PROJECT

helmr task start review-pr \
  --payload-json '{"owner":"OWNER","repo":"REPO","prNumber":123}'
```

If a task needs repository files, clone or fetch them from inside the task using
payload fields and declared secrets. Helmr keeps the runtime substrate generic;
GitHub is a task integration, not a required run source.

## Payloads and secrets

Payload is audit data. Helmr persists it in plaintext in the database, run
events, and event streams. Do not put tokens, API keys, credentials, or sensitive
personal data in payloads.

Tasks declare the Helmr secret names they need and where each value appears
inside the guest, such as an environment variable:

```sh
printf '%s' "$OPENAI_API_KEY" | helmr secret set OPENAI_API_KEY
helmr task start my-task
```

Runs never receive secret values or binding maps. The deployed task definition is
the contract; Helmr resolves declared secret names from the selected project
environment when the run starts.

## Checkpoint encryption

Checkpoint artifacts are encrypted before leaving the worker staging directory.
Workers require `HELMR_CHECKPOINT_ENCRYPTION_KEY`, a base64-encoded 32-byte key.
Use the same key for workers that must restore the same checkpoint state:

```sh
head -c 32 /dev/urandom | base64
```

## Development

Common local checks:

```sh
make test
make lint
make build
bun run typecheck
```

CI parity and platform-specific checks:

```sh
nix flake check
nix run .#ci-checks
nix run .#ci-policy
nix run .#ci-generated
nix run .#ci-typescript
nix run .#ci-go-test
nix run .#ci-go-lint
nix run .#ci-go-build
nix run .#ci-go-race
nix run .#ci-linux-compile
nix run .#ci-linux-lint
nix run .#ci-postgres
nix run .#ci-buildkit
```

On Linux, `nix flake check` also evaluates the Firecracker host NixOS module.

Linux Firecracker smoke tests need a Linux host with KVM:

```sh
nix run .#smoke-linux
```

## More documentation

- [SDK](sdk/) - TypeScript SDK and runtime client notes
- [Runtime](runtime/) - guest-side runtime adapter responsibilities
- [Examples](examples/) - runnable task projects
- [AWS infrastructure](infra/aws/) - self-hosted AWS modules and dev smoke flow
- [Scripts](scripts/) - maintenance and CI helper entrypoints
- [Images](images/) - guest boot artifact recipes
- [Proto](proto/) - protocol definitions and generated bindings

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
