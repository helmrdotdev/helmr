---
title: helmr login, logout, whoami and completion
description: Authenticate the CLI, inspect the active credentials, and generate shell completion.
sidebarLabel: login
---

# `helmr login`, `logout`, `whoami` and `completion`

```text
helmr login [URL] [--no-browser]
helmr logout [URL]
helmr whoami [--json]
helmr completion SHELL
```

For first-time setup, see [Quickstart](/docs/quickstart).

## `helmr login`

Starts a device authorization request, prints the verification URL and a code,
opens the URL in a browser, and waits for approval. On approval it saves the
login for that control plane origin.

| Argument or flag | Description |
| --- | --- |
| `URL` | Control plane origin. Cannot be combined with `--api-url`. |
| `--no-browser` | Print the URL without opening a browser. |

The origin is chosen from, in order: `URL` or `--api-url`, `HELMR_API_URL`, the
saved default origin, then `https://helmr.dev`, the planned managed service.
A self-hosted control plane needs its origin passed explicitly the first time. The origin must be `https`,
or `http` on a loopback host, with no path, query, fragment or credentials.

The browser page shows the account, organization, server and code to approve.
An expired or denied request needs a new `helmr login`. CLI login does not
require a Project; commands that act in an Environment still need
`--project` and `--env`.

## `helmr logout`

Revokes the saved login for an origin and deletes it locally. The origin is
chosen from `URL` or `--api-url`, `HELMR_API_URL`, then the saved default
origin. Fails with `not logged in to ORIGIN` when no login is saved for it.

## `helmr whoami`

Reports which credentials the CLI would use.

| Output | Description |
| --- | --- |
| Text | `AUTH_SOURCE ORIGIN`, where `AUTH_SOURCE` is `api_key` or `login`. |
| `--json` | One object with `auth_source` and `api_url`. For a saved login it also includes the account fields: `user_id`, `display_name`, `profile_image_url`, `public_url`, `org_id`, `org_name`, `org_slug`, `role`, `admin`, `permissions`, `organization_required` and `project_required`, when present. |

## Credential selection

| Condition | Credentials | Origin |
| --- | --- | --- |
| `HELMR_API_KEY` is set | The API key | `--api-url`, then `HELMR_API_URL`, then the built-in default `https://api.helmr.dev` (the planned managed service). Set `HELMR_API_URL` for a self-hosted control plane. |
| Otherwise | The saved login for the origin | `--api-url`, then `HELMR_API_URL`, then the saved default origin |

`HELMR_API_KEY` must not have surrounding whitespace. An API key is bound to one
Environment, so `--project` and `--env` are rejected with it. With a saved
login, Environment-scoped commands require both. Project and Environment
management (`helmr project`, `helmr env`) always uses the saved login.

## Saved state

| Item | Location |
| --- | --- |
| Default origin | `config.toml` in the CLI configuration directory |
| Login token | The operating system keyring, under the service `helmr-cli` |

The configuration directory is `HELMR_CONFIG_DIR` when set, otherwise
`$XDG_CONFIG_HOME/helmr`, otherwise `%AppData%\helmr` on Windows and
`~/.config/helmr` elsewhere. See [Environment variables](/docs/reference/env).

## `helmr completion`

Generates a shell completion script for `bash`, `zsh`, `fish` or `powershell`.
Run `helmr completion SHELL --help` for installation instructions.
