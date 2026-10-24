---
title: CLI
description: Global flags, credentials and the full helmr command list.
sidebarLabel: Overview
---

# CLI

`helmr` builds and deploys Agent projects and calls the control plane over
HTTP(S). With the CLI configured (see [Quickstart](/docs/quickstart)), run
`helmr COMMAND --help` for the exact flags of the installed version.

## Global flags

| Flag | Description |
| --- | --- |
| `-a, --api-url URL` | Control plane origin for this invocation. Overrides `HELMR_API_URL`. |
| `--help` | Show help for any command. |
| `--version` | Print the CLI version. |

## Credentials and scope

The CLI uses `HELMR_API_KEY` when it is set, and otherwise the saved login for
the selected origin. An API key is bound to one Environment, so `-p, --project`
and `-e, --env` are rejected with it. With a saved login, commands that act in
an Environment require both flags; each accepts the public ID shown in the Console. Details are in
[Credential selection](/docs/reference/cli/login#credential-selection).

## Commands

| Command | Subcommands | Description | Reference |
| --- | --- | --- | --- |
| `helmr init` | | Create a starter Agent project. | [init](/docs/reference/cli/init) |
| `helmr login` | | Authenticate the CLI with a control plane. | [login](/docs/reference/cli/login) |
| `helmr logout` | | Revoke and delete a saved login. | [login](/docs/reference/cli/login#helmr-logout) |
| `helmr whoami` | | Show the active credentials. | [login](/docs/reference/cli/login#helmr-whoami) |
| `helmr status` | | Show CLI context and existing resources. | [status](/docs/reference/cli/status) |
| `helmr doctor` | | Diagnose existing resources. | [doctor](/docs/reference/cli/status) |
| `helmr build` | | Build a verified deployment bundle locally. | [build](/docs/reference/cli/build) |
| `helmr deploy` | | Build, upload, finalize and promote a deployment bundle. | [deploy](/docs/reference/cli/deploy) |
| `helmr deployment` | `list`, `get`, `promote` | Inspect Deployments and promote one to current. | [deployment](/docs/reference/cli/deployment) |
| `helmr agent` | `list`, `get` | Inspect deployed Agents. | [agent](/docs/reference/cli/agent) |
| `helmr session` | `create`, `list`, `get`, `enqueue`, `events`, `interrupt`, `resume`, `close`, `cancel`, `turn list`, `turn get`, `turn wait`, `turn send`, `turn interrupt`, `turn ask list`, `turn ask get`, `turn ask answer` | Work with Sessions, Turns and questions. | [session](/docs/reference/cli/session) |
| `helmr trigger` | `list`, `get` | Inspect Triggers created by triggers. | [trigger](/docs/reference/cli/trigger) |
| `helmr template` | `list`, `get` | Inspect deployed Templates. | [template](/docs/reference/cli/template) |
| `helmr computer` | `create`, `list`, `get`, `sessions`, `commands`, `update`, `delete`, `exec` | Work with Computers and their current usage. | [computer](/docs/reference/cli/computer) |
| `helmr command` | `get`, `wait`, `logs`, `cancel` | Observe and cancel Commands. | [command](/docs/reference/cli/command) |
| `helmr project` | `list`, `get`, `create`, `update` | Manage Projects. Requires a saved login. | [project](/docs/reference/cli/project) |
| `helmr env` | `list`, `get`, `create`, `update` | Manage a Project's Environments. Requires a saved login. | [project](/docs/reference/cli/project) |
| `helmr secret` | `list`, `get`, `create`, `rotate`, `revoke` | Manage Environment Secrets. | [secret](/docs/reference/cli/secret) |
| `helmr role` | `list`, `get`, `create`, `update`, `copy`, `delete`, `grant list/get/create/delete` | Define Roles and assign scoped grants. | [permissions](/docs/reference/cli/permissions) |
| `helmr group` | `list`, `get`, `create`, `update`, `delete`, `member list/add/remove` | Manage permission Groups. | [permissions](/docs/reference/cli/permissions) |
| `helmr member` | `permissions` | Inspect a member’s effective permissions. | [permissions](/docs/reference/cli/permissions) |
| `helmr completion` | `bash`, `zsh`, `fish`, `powershell` | Generate a shell completion script. | [login](/docs/reference/cli/login#helmr-completion) |

## Output

Most commands print `key: value` lines or one line per item. Commands with
`--json` print the server resource or receipt as one JSON object; some list
commands also offer `--jsonl`. Receipts confirm that the control plane accepted
a request, not that the work has finished.

## Environment variables

| Variable | Description |
| --- | --- |
| `HELMR_API_KEY` | API key for workload commands. Management commands use the saved login. |
| `HELMR_API_URL` | Control plane origin when `--api-url` is not given. |
| `HELMR_CONFIG_DIR` | Directory for saved CLI configuration. |

See [Environment variables](/docs/reference/env) for the complete list.
