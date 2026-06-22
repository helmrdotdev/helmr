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

`HELMR_API_KEY` may be either an environment-bound API key or a session token.
API keys use root API-key routes; session tokens use
`HELMR_PROJECT`/`HELMR_ENV` scoped routes.

Set `SKIP_DEPLOY=1` to reuse the currently deployed `dev/workflows` task
project. The smoke creates a direct workspace from the `runtime-smoke` sandbox,
materializes it, then attaches `runtime-smoke` task runs to that workspace as
the VM-side probe.

## Workspace Primitive Smokes

Workspace primitive diagnostics are split by lifecycle authority:

```sh
bun run --cwd dev/client workspace:exec
bun run --cwd dev/client workspace:pty
bun run --cwd dev/client workspace:stop
bun run --cwd dev/client workspace:files
bun run --cwd dev/client workspace:ports
```

`workspace:exec` creates a direct workspace, materializes it, starts a real
workspace exec, follows stdout/stderr through server push, writes and closes
stdin, waits for the durable exit code, and verifies retained replay after the
stream completes.

`workspace:pty` creates a direct workspace, materializes it, opens a PTY,
follows output through server push, writes input, resizes, closes, and verifies
retained replay after terminal close.

`workspace:stop` creates a direct workspace, writes a marker through a real
workspace exec, calls `workspace.stop()`, waits for no active materialization,
rematerializes the workspace, and verifies the marker was captured into the
promoted system workspace version.

`workspace:files` and `workspace:ports` remain planned-surface diagnostics until
their public SDK and REST primitives are implemented.
