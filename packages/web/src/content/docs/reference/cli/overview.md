---
title: CLI
description: Authentication, global flags, and command groups for helmr.
sidebarLabel: Overview
---

# CLI

`helmr` talks to the Control Plane over HTTP(S). `-a, --api-url` overrides the
origin. API-key commands otherwise use `HELMR_API_URL` or
`https://api.helmr.dev`; session commands use the origin saved by `helmr login`.

Authenticate with `HELMR_API_KEY` or `helmr login [URL] [--no-browser]`.
`helmr logout [URL]` revokes a saved session and `helmr whoami [--json]` reports
the active source. `HELMR_CONFIG_DIR` overrides the saved CLI state directory.

Browser login returns you to the CLI approval request after any required sign-in
or organization setup. Review the account, organization, server, and terminal
code before approving. CLI login does not require a project; commands that need
an environment still require project and environment scope. The browser confirms
approval, and the terminal confirms login after saving the credentials.

An expired or denied request requires a new `helmr login`. Starting another GitHub
sign-in supersedes the earlier browser attempt; completing the earlier callback
does not cancel the newer one. Organization switching is not supported by this flow.

If browser sign-in fails during CLI login, run `helmr login` again to start a new
request. The callback error page also offers a separate link to sign in to the
console.

Top-level commands also include `init [--dir DIR] [--force]`, `completion`,
`--help`, and `--version`. Use `helmr COMMAND --help` as executable authority
for the installed CLI version.

Environment-scoped commands accept `-p, --project` and `-e, --env`. Saved-login
requests require both. An environment-bound API key supplies that scope, so
project and environment flags are rejected with API-key auth.
