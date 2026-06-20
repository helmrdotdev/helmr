# Helmr Dev Client

This directory contains reusable client-side diagnostics for Helmr API and SDK
behavior. These scripts are not deployed as task workflows; they run from a
developer machine against a control plane such as the AWS dev stack.

## Workspace Lifecycle Smoke

The workspace lifecycle smoke exercises the SDK client surface for durable
workspace APIs and task attachment:

- `workspaces.create`
- `workspaces.open`
- `workspace.retrieve`
- `workspace.update`
- `workspace.materialize`
- `workspace.connect`
- `tasks.start` with `workspaceId`
- `sessions.retrieve`
- `runs.wait`

Run from the repository root:

```sh
HELMR_API_URL=https://dev.helmr.dev \
HELMR_API_KEY=... \
dev/client/scripts/workspace-lifecycle-smoke.sh
```

Set `SKIP_DEPLOY=1` to reuse the currently deployed `dev/workflows` task
project. The smoke creates a direct workspace from the `runtime-smoke` sandbox,
materializes it, then attaches `runtime-smoke` task runs to that workspace as
the VM-side probe.

## Workspace Primitive Smokes

Workspace primitive diagnostics are split by lifecycle authority:

```sh
bun run --cwd dev/client workspace:exec
bun run --cwd dev/client workspace:pty
bun run --cwd dev/client workspace:files
bun run --cwd dev/client workspace:ports
```

These currently report `not_implemented` because the corresponding public SDK
and REST surfaces are not implemented yet. They are separate entrypoints so the
Phase 7B+ implementation can fill in each primitive without overloading the
lifecycle smoke.
