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

Top-level commands also include `init [--dir DIR] [--force]`, `completion`,
`--help`, and `--version`. Use `helmr COMMAND --help` as executable authority
for the installed CLI version.

Environment-scoped commands accept `-p, --project` and `-e, --env`. Saved-login
requests require both. An environment-bound API key supplies that scope, so
project and environment flags are rejected with API-key auth.
