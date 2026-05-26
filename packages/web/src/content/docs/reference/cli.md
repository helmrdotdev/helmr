---
title: CLI reference
description: Commands and environment used by the helmr CLI.
section: Reference
sidebarLabel: CLI
order: 900
---

# CLI reference

The `helmr` CLI talks to the control plane over HTTP(S). API access uses `HELMR_URL` plus `HELMR_API_KEY`, or a saved login from `helmr login`.

| Command | Purpose |
| --- | --- |
| `helmr init [--dir DIR] [--force]` | Create `package.json`, `helmr.config.ts`, and `tasks/hello.ts`. |
| `helmr login [URL] [--url URL] [--no-browser]` | Start device-code auth and save a session token. Defaults to `HELMR_URL`, saved host, or `https://helmr.dev`. |
| `helmr logout [URL]` | Revoke the current saved session token for a host. |
| `helmr deploy [path] [--environment ID]` | Parse `helmr.config.ts`, archive source, send its content hash, and create a deployment for the configured project. |
| `helmr run TASK --repo OWNER/REPO --ref REF` | Create a GitHub-backed run. |
| `helmr ps [--json]` | List runs. |
| `helmr show RUN [--json]` | Show run details. |
| `helmr logs RUN` | Print latest stdout and stderr snapshots. |
| `helmr events RUN [--cursor N] [--limit N]` | Print run events as JSON lines. |
| `helmr secret set NAME [VALUE]` | Create or update a remote secret; reads stdin if value is omitted. |
| `helmr resume approve|deny RUN WAITPOINT [--reason TEXT]` | Resolve an approval waitpoint. |
| `helmr resume message RUN WAITPOINT --text TEXT` | Reply to a message waitpoint. |

`helmr run` accepts payloads from `--payload-file`, `--payload-json`, or repeated `-p/--payload KEY=VALUE`. Secret bindings use `--secret NAME=vault:SECRET_NAME`.
