# Helmr

Helmr is a self-hosted runtime for coding agents.

It provides the infrastructure around an agent SDK: a real GitHub checkout, an
isolated filesystem, controlled credentials, logs, run history, and approval
points before a task writes back. Task code is written in TypeScript and runs
inside Firecracker-backed Linux guests managed by your own control plane and
workers.

## Status

Helmr is in early active development. APIs, deployment shape, and operational
defaults may change before a stable release. The current codebase is best suited
for contributors, early adopters, and self-hosted evaluation.

## What Helmr provides

- TypeScript tasks that declare images, sandboxes, resources, secrets, inputs,
  and run logic
- GitHub checkouts mounted inside isolated Linux guests
- Approval waitpoints before reviews, patches, or other side effects
- Run status, logs, events, payloads, and history in the control plane
- Task-declared secrets injected only at run time
- A runtime boundary you own: your AWS account, your GitHub App, your workers
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
human approval points. The code inside the task can call any agent SDK or tool;
Helmr owns the adapter protocol around it.

Create a task project with `helmr.config.ts` and one or more task modules:

```ts
import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

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
  secrets: {
    OPENAI_API_KEY: { env: "OPENAI_API_KEY" },
  },
  run: async (event: { prNumber: number }, ctx) => {
    // Call your agent SDK or review tooling here.
    const summary = await reviewPullRequest({
      cwd: process.cwd(),
      prNumber: event.prNumber,
      token: process.env.OPENAI_API_KEY ?? "",
    })

    const decision = await ctx.wait.manual<{ approved: boolean }>({
      displayText: "Post this review to GitHub?",
    })
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

Tasks start in the checked-out workspace directory. Use relative paths for
workspace files; absolute paths keep normal Linux container semantics.

See [examples/](examples/) for deployable task projects, including dependency
caching, CLI tooling, human input waitpoints, vault secrets, and GitHub PR
review flows.

## Run against GitHub

Remote runs use a deployment task source and a GitHub workspace:

```sh
helmr deploy PATH/TO/TASK_PROJECT

helmr run review-pr \
  --repo OWNER/REPO \
  --ref main \
  --subpath PATH/TO/TASK_PROJECT \
  --payload-json '{"prNumber":123}' \
  --secret OPENAI_API_KEY=vault:OPENAI_API_KEY
```

The workspace repository must be accessible to the Helmr GitHub App configured
for your control plane. When `--subpath` is set, that directory is materialized
as the workspace root in the sandbox.

## Payloads and secrets

Payload is audit data. Helmr persists it in plaintext in the database, run
events, and event streams. Do not put tokens, API keys, credentials, or sensitive
personal data in payloads.

Tasks declare where secrets appear inside the guest, such as an environment
variable. Remote runs bind those declarations to vault references:

```sh
printf '%s' "$OPENAI_API_KEY" | helmr secret set OPENAI_API_KEY
helmr run my-task --secret OPENAI_API_KEY=vault:OPENAI_API_KEY
```

Remote runs reject local-only secret sources such as `env:` and `file:`.

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
nix run .#ci-postgres
nix run .#ci-buildkit
make test-linux-compile
```

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
