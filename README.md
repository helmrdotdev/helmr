# Helmr

**Build your own software factory.**

Infrastructure and APIs for your own agent harness.<br>
Your agents. Your workflows. Your rules.

Write your agent logic in TypeScript, bring your tools and integrations, and
run it in isolated Linux microVMs. Keep your computer across turns, pause for
human input, and inspect what happened.

## What you get

- **Persistent computers** — files and dependencies kept across turns.
- **Agents and sessions** — define agent behavior and continue its work across turns.
- **Human input** — pause for approval or external input, then continue.
- **Secrets and visibility** — runtime secret injection, logs, and session history.
- **Infrastructure you control** — self-host in your own AWS account.

## Get started

Install the CLI:

```sh
curl -fsSL https://helmr.dev/install | bash
```

Follow the [quickstart](https://helmr.dev/docs/quickstart/) to deploy and run your
first agent. You'll need a running control plane and worker; the
[self-hosting guide](https://helmr.dev/docs/self-hosting/overview/) covers setup.

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

- [Documentation](https://helmr.dev/docs/)
- [TypeScript SDK](https://helmr.dev/docs/reference/sdk/overview/)
- [REST API](https://helmr.dev/docs/reference/rest-api/overview/)
- [Examples](examples/)
- [Development verification](tests/e2e/README.md)

Early, active development. APIs and deployment details may change before a
stable release.

[Apache 2.0](LICENSE).
