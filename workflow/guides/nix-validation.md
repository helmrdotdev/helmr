# Nix Validation Guide

All repository development tools are managed by Nix.

## Default Rule

Run development, format, generation, typecheck, test, lint, and build commands through `nix develop ... -c`.

Use this default shape for ordinary checks:

```sh
nix develop --accept-flake-config -c <command>
```

Use narrower shells when the repository defines them:

```sh
nix develop .#images --accept-flake-config -c <command>
nix develop .#infra --accept-flake-config -c <command>
```

## Direct Tool Calls

Do not call `go`, `gofmt`, `goimports`, `bun`, `buf`, `make`, `docker`, `tofu`, or `aws` directly unless the command is already running inside a Nix develop shell.

## Reporting

For each validation command, report:

- Working directory.
- Exact command.
- Exit status.
- Why the check was relevant.
- Result summary.

If a check cannot run, report the command attempted, the failure, and what remains unverified.
