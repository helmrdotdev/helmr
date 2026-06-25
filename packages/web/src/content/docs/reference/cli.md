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
| `helmr task start TASK [-p PROJECT] [-e ENV] [--json]` | Start a task session for a deployed task. |
| `helmr session list` | List task sessions. |
| `helmr session get SESSION` | Show task session details. |
| `helmr session wait SESSION` | Wait for a task session to finish. |
| `helmr session cancel SESSION` | Cancel a task session. |
| `helmr session input send SESSION STREAM --data-json JSON` | Append a session input record. |
| `helmr session output list SESSION STREAM` | List retained session stream output. |
| `helmr run list [--session SESSION] [--json]` | List run attempts. |
| `helmr run get RUN [--json]` | Show run details. |
| `helmr run logs RUN [--follow]` | Print latest stdout/stderr snapshots and optionally stream new log chunks. |
| `helmr run events RUN [--cursor N] [--limit N] [--follow]` | Print run events as JSON lines. |
| `helmr run wait RUN [--timeout DURATION] [--json]` | Wait for a run to finish using the run event stream. |
| `helmr run cancel RUN [--idempotency-key KEY]` | Cancel a run attempt. |
| `helmr workspace create` | Create a durable workspace. |
| `helmr workspace list` | List durable workspaces. |
| `helmr workspace get WORKSPACE` | Show workspace details. |
| `helmr workspace update WORKSPACE` | Update workspace metadata. |
| `helmr workspace delete WORKSPACE` | Delete a workspace. |
| `helmr workspace open WORKSPACE` | Print the workspace console URL. |
| `helmr workspace materialize WORKSPACE` | Ensure a live workspace materialization exists. |
| `helmr workspace connect WORKSPACE` | Connect to a live workspace materialization. |
| `helmr workspace stop WORKSPACE` | Stop the live materialization while keeping the durable workspace. |
| `helmr workspace exec WORKSPACE -- COMMAND [ARGS...]` | Run a command in a workspace. |
| `helmr workspace exec list WORKSPACE` | List workspace exec records. |
| `helmr workspace exec get WORKSPACE EXEC` | Show workspace exec details. |
| `helmr workspace exec logs WORKSPACE EXEC` | Read workspace exec stdout/stderr. |
| `helmr workspace exec wait WORKSPACE EXEC` | Wait for a workspace exec to finish. |
| `helmr workspace shell WORKSPACE` | Open an interactive shell in a workspace. |
| `helmr workspace pty create WORKSPACE` | Create a workspace PTY session. |
| `helmr workspace pty connect WORKSPACE PTY` | Connect to a workspace PTY session. |
| `helmr workspace pty close WORKSPACE PTY` | Close a workspace PTY session. |
| `helmr deployment list` | List deployments. |
| `helmr deployment get DEPLOYMENT` | Show deployment details. |
| `helmr sandbox list` | List deployed sandbox definitions. |
| `helmr sandbox get SANDBOX` | Show deployed sandbox details. |
| `helmr secret list [--json]` | List remote secret metadata. |
| `helmr secret get NAME [--json]` | Show remote secret metadata. Secret values are never returned. |
| `helmr secret set NAME [VALUE] [--json]` | Create or update a remote secret; reads stdin if value is omitted. |
| `helmr secret delete NAME --yes` | Delete a remote secret. |

Common options:

| Option | Purpose |
| --- | --- |
| `-a, --api-url URL` | Override the Helmr control API URL. |
| `--help` | Show command help. |
| `--version` | Print the CLI version. |

`helmr deploy` writes human-readable progress to stderr and the final deployment version or ID to stdout. With `--json`, it emits JSON lines for local steps, deployment events, and the final deployment result.

`helmr task start` accepts payloads from `--payload-file`, `--payload-json`, or repeated `--payload KEY=VALUE`. `-p` is reserved for `--project`. Use `--workspace WORKSPACE_ID` to attach the new task session to an existing durable workspace. Secrets are declared by deployed task source and resolved from the selected project environment at run time.

`helmr run wait` follows durable run events and reconnects with the last event cursor. It no longer polls on an interval.

`helmr run logs --follow` prints the current log snapshot, then follows the dedicated run log stream. It reconnects with the last log cursor and exits after the run reaches a terminal state.

`helmr workspace exec` uses `--` before the remote command. Foreground exec streams stdout/stderr and exits with the remote process exit code. `--detach` returns the exec handle without waiting.

`helmr session input send SESSION STREAM --data-json JSON` appends a record to a named session input stream. `helmr session output list SESSION STREAM` reads retained output records.
