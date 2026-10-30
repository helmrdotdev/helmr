---
title: helmr login, logout, whoami and completion
description: Authenticate the CLI, inspect the active credentials, and generate shell completion.
sidebarLabel: login
---

# `helmr login`, `logout`, `whoami` and `completion`

```text
helmr login [URL] [--no-browser] [--confirm]
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
| `--confirm` | Confirm the saved CLI login in the browser for sensitive changes. Requires an existing login. |

The origin is chosen from, in order: `URL` or `--api-url`, `HELMR_API_URL`, the
saved default origin, then `https://helmr.dev`, the planned managed service.
A self-hosted control plane needs its origin passed explicitly the first time. The origin must be `https`,
or `http` on a loopback host, with no path, query, fragment or credentials.

The browser page shows the account, organization, server and code to approve.
An expired or denied request needs a new `helmr login`. CLI login does not
require a Project; commands that act in an Environment still need
`--project` and `--env`, or `HELMR_PROJECT` and `HELMR_ENV`.

### Confirming sensitive changes

A CLI login is bound to the account and organization approved in the browser.
To authorize another organization, run `helmr login` again and approve that
organization.

When a sensitive command requires fresh authentication, an interactive terminal
opens the browser confirmation flow. You can also start it explicitly:

```sh
helmr login --confirm
# After browser confirmation, run your original command again.
```

Confirmation replaces the existing CLI session for the same account and organization.
Queued work and open streams using the old session lose access at their next
authority check. Commands are never automatically replayed after confirmation.
Non-interactive commands print guidance instead of opening confirmation; JSON
output retains the error without starting an interactive flow.
`--no-browser` can be combined with `--confirm` to print the approval URL.

CLI sessions expire after thirty days without explicit human activity. Successful
commands in an interactive terminal record activity; background polling does not
extend the expiry. Browser logout does not log out the CLI, but account-wide
revocation and removing the sign-in method used to approve the CLI can retire CLI access.

## `helmr logout`

Revokes the saved login for an origin and deletes it locally. The origin is
chosen from `URL` or `--api-url`, `HELMR_API_URL`, then the saved default
origin. Fails with `not logged in to ORIGIN` when no login is saved for it.

## `helmr whoami`

Reports which credentials the CLI would use.

| Output | Description |
| --- | --- |
| Text | `AUTH_SOURCE ORIGIN`, where `AUTH_SOURCE` is `api_key` or `login`. Saved logins also show account and organization names and IDs. |
| `--json` | One object with `auth_source` and `api_url`. A saved login also includes `organization`, containing `id`, `name`, `slug`, `current_member`, `authentication` and `onboarding`. `current_member.user_id` identifies the account; `current_member.id` identifies its organization membership. API-key output omits `organization`. |

## Credential selection

| Condition | Credentials | Origin |
| --- | --- | --- |
| `HELMR_API_KEY` is set | The API key | `--api-url`, then `HELMR_API_URL`, then the built-in default `https://api.helmr.dev` (the planned managed service). Set `HELMR_API_URL` for a self-hosted control plane. |
| Otherwise | The saved login for the origin | `--api-url`, then `HELMR_API_URL`, then the saved default origin |

`HELMR_API_KEY` must not have surrounding whitespace. An API key is bound to one
Environment, so `--project` and `--env` are optional with it; a value given
(including `HELMR_PROJECT`/`HELMR_ENV`) must match the key's own Project or
Environment, or the server rejects the request with `api_key_scope_mismatch`.
With a saved login, Environment-scoped commands require both; each flag falls
back to its environment variable when absent. Project and Environment
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
