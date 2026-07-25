---
title: CLI reference
description: Commands and environment used by the helmr CLI.
section: Reference
sidebarLabel: CLI
order: 900
---

# CLI reference

The `helmr` CLI talks to the control plane over HTTP(S). Choose the endpoint with `--api-url`, `HELMR_API_URL`, or a saved login from `helmr login`. Authenticate with `HELMR_API_KEY` or a saved login session.

| Command | Purpose |
| --- | --- |
| `helmr init [--dir DIR] [--force]` | Create `package.json`, `helmr.config.ts`, and `tasks/hello.ts`. |
| `helmr login [URL] [--no-browser]` | Start device-code auth and save a session token. Defaults to `--api-url`, `HELMR_API_URL`, saved host, or `https://helmr.dev`. |
| `helmr logout [URL]` | Revoke the current saved session token for a host. |
| `helmr deploy [path] [-p PROJECT] [-e ENV] [--env-file FILE] [--timeout DURATION] [--json]` | Parse `helmr.config.ts`, archive source, stream deployment progress, and create a deployment. |
| `helmr task list [--json]` | List deployed task definitions. |
| `helmr task get TASK [--json]` | Show a deployed task definition. |
| `helmr run start TASK --workspace WORKSPACE [-p PROJECT] [-e ENV] [--wait] [--follow] [--json]` | Start a deployed Task Run in an existing Workspace. |
| `helmr run list [-p PROJECT] [-e ENV] [--json]` | List Runs. |
| `helmr run get RUN [-p PROJECT] [-e ENV] [--json]` | Show run details. |
| `helmr run logs RUN [-p PROJECT] [-e ENV] [--follow]` | Print latest stdout/stderr snapshots and optionally stream new log chunks. |
| `helmr run events RUN [-p PROJECT] [-e ENV] [--cursor CURSOR] [--limit N] [--follow]` | Print run events as JSON lines. |
| `helmr run wait RUN [-p PROJECT] [-e ENV] [--timeout DURATION] [--json]` | Wait for a run to finish using the run event stream. |
| `helmr run cancel RUN [-p PROJECT] [-e ENV] [--reason TEXT] [--force] [--idempotency-key KEY] [--json]` | Cancel a run attempt. |
| `helmr token get TOKEN [-p PROJECT] [-e ENV] [--json]` | Show an external completion token. |
| `helmr token complete TOKEN [-p PROJECT] [-e ENV] --data-json JSON [--idempotency-key KEY] [--json]` | Complete an external token. |
| `helmr token cancel TOKEN [-p PROJECT] [-e ENV] [--idempotency-key KEY] [--json]` | Cancel a pending external token. |
| `helmr workspace create DECLARED_ID` | Create a durable Workspace. |
| `helmr workspace get --id WORKSPACE` | Show Workspace details. |
| `helmr workspace delete --id WORKSPACE` | Delete a Workspace. |
| `helmr workspace files read/list/stat ...` | Read the current committed filesystem. |
| `helmr workspace exec --id WORKSPACE --idempotency-key KEY -- COMMAND [ARGS...]` | Run one bounded command and return its terminal output. |
| `helmr deployment list` | List deployments. |
| `helmr deployment get DEPLOYMENT` | Show deployment details. |
| `helmr secret list [--json]` | List remote secret metadata. |
| `helmr secret get NAME [--json]` | Show remote secret metadata. Secret values are never returned. |
| `helmr secret create NAME [VALUE] [--json]` | Create a remote secret; reads stdin if value is omitted. |
| `helmr secret rotate NAME [VALUE] [--json]` | Add a new immutable version behind a stable Secret; reads stdin if value is omitted. |
| `helmr secret revoke NAME --yes` | Revoke a remote secret. |

Common options:

| Option | Purpose |
| --- | --- |
| `-a, --api-url URL` | Override the Helmr control API URL. |
| `--help` | Show command help. |
| `--version` | Print the CLI version. |

`helmr deploy` writes human-readable progress to stderr and the final deployment version or ID to stdout. With `--json`, it emits JSON lines for local steps, deployment events, and the final deployment result.

`helmr run start` accepts payloads from `--payload-file`, `--payload-json`, or
repeated `--payload KEY=VALUE`. `--workspace` is required. Secrets are declared
in source and placed when the Workspace is created. `--wait` waits for the Run
to finish.

With saved login auth, environment-scoped commands require both `--project` and `--env`. With `HELMR_API_KEY`, the key is already bound to one environment and project/environment flags are rejected.

`helmr run wait` polls finite Run snapshots and telemetry pages until the Run is terminal.

`helmr run logs --follow` prints finite log pages from the last opaque cursor and exits after the Run reaches a terminal state.

`helmr workspace exec` uses `--` before the remote command and requires an
idempotency key. It returns bounded stdout/stderr and exits with the remote
process exit code.
