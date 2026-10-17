# Helmr

**Build cloud agents that work your way.**

Your models, tools, and code. Helmr handles the infrastructure.

Define an agent's work in plain TypeScript and run it with the coding agent you
already use: Claude Code, Codex, OpenCode, Pi, Cursor or the AI SDK. Helmr gives it
a computer you define in code, keeps its work through hibernation and resume, and
lets it work alongside your team in Slack.

## What you get

- **Computers defined in code**: image, tools, resources, prepare steps and secrets in one definition.
- **Agents in plain TypeScript**: start your coding agent once in `init`, then hand it each turn.
- **Immediate replies, background file saves**: completed turns have durable results and conversation records. Computer files save in the background. If a worker is lost before files are saved, Helmr warns each affected conversation and waits for your next message or Resume before continuing from the last retained save, or the initial work environment if no save exists. Completed turns are never replayed.
- **People in the loop**: agents ask questions with `turn.ask`, and teammates answer from Slack or the CLI.
- **Protected secrets**: real credentials are swapped in outside the VM, only for the origins you allow.
- **Infrastructure you control**: self-host in your own AWS account. Managed Helmr Cloud is planned.

## Get started

Install the CLI:

```sh
curl -fsSL https://helmr.dev/install | bash
```

Follow the [quickstart](https://helmr.dev/docs/quickstart) to deploy and run your
first agent. You need a running control plane and worker; the
[self-hosting guide](https://helmr.dev/docs/self-hosting/overview) covers setup.

## Build from source

Build the CLI with the pinned toolchain and generated platform entries:

```sh
nix build .#helmr
```

For local development, install workspace dependencies and use the Make targets:

```sh
nix develop -c bun install --frozen-lockfile --ignore-scripts
nix develop -c make build
```

`make build`, `make test` and `make lint` generate the JavaScript entries before
their Go consumers. Before running focused Go commands, run
`nix develop -c make platform-entries` and repeat it after TypeScript or dependency
changes. The four platform `.mjs` bundles are local build outputs, not source
files; a fresh checkout needs this preparation for direct Go builds.
Go-only `go install ...@version` is not a supported source installation path.

Nix acquires hash-verified dependencies separately from offline entry generation.
Dependency updates may require updating the dependency output hash in
`nix/packages/platform-entries.nix`; the esbuild API and native fetch versions
must agree with `package.json` and `bun.lock`. CI compares all four Nix-generated
entries with local generation byte-for-byte.

## Explore

- [Documentation](https://helmr.dev/docs)
- [How Helmr works](https://helmr.dev/docs/concepts/how-helmr-works)
- [TypeScript SDK](https://helmr.dev/docs/reference/sdk/overview)
- [REST API](https://helmr.dev/docs/reference/api/overview)
- [Examples](examples/)

Early, active development. APIs and deployment details may change before a
stable release.

[Apache 2.0](LICENSE).
