---
title: REST API
description: Control-plane API routes used by the CLI, SDK, web UI, and workers.
section: Reference
sidebarLabel: REST API
order: 940
---

# REST API

The control plane serves JSON APIs under `/api`. Authenticated user/API-key requests use `Authorization: Bearer TOKEN`. Worker requests use worker bearer tokens minted by `/api/worker/auth/token`.

## API version header

User, API-key, console, CLI, SDK, and worker API requests use a date-pinned API contract header:

```http
Helmr-API-Version: 2026-06-06
```

The date is a fixed build constant, not the request date. The control plane echoes the effective version in `Helmr-API-Version`. Requests with an unsupported non-empty version return `400 Bad Request`; omitted versions currently default to the current version during pre-release development. Header values are exact and are not trimmed.

Client provenance headers are separate from the API contract:

| Header | Meaning |
| --- | --- |
| `Helmr-Client-Version` | Generic client build version. |
| `Helmr-CLI-Version` | CLI build version for CLI-originated requests. |
| `Helmr-SDK-Version` | SDK package version for SDK-originated requests. |

These provenance headers are recorded on deployments and runs where available. They are opaque diagnostic metadata rather than SemVer or compatibility gates. A value must be valid UTF-8, have no surrounding whitespace or control characters, and be at most 255 bytes. Empty means unknown; invalid values return `400 Bad Request`.

Common user/API-key routes:

| Method | Path |
| --- | --- |
| `POST` | `/api/sessions` |
| `POST` | `/api/sessions/start-and-wait` |
| `GET` | `/api/sessions` |
| `GET` | `/api/sessions/{id}` |
| `PATCH` | `/api/sessions/{id}` |
| `POST` | `/api/sessions/{id}/close` |
| `POST` | `/api/sessions/{id}/cancel` |
| `GET` | `/api/sessions/{id}/runs` |
| `POST` | `/api/actors/{actorDeclaredID}/start` |
| `POST` | `/api/actors/{actorDeclaredID}/input` |
| `GET` | `/api/actors/{actorDeclaredID}/output` |
| `GET` | `/api/actors/{actorDeclaredID}/status` |
| `POST` | `/api/actors/{actorDeclaredID}/close` |
| `GET` | `/api/runs` |
| `GET` | `/api/runs/counts` |
| `GET` | `/api/runs/{id}` |
| `GET` | `/api/runs/{id}/events` |
| `GET` | `/api/runs/{id}/logs` |
| `POST` | `/api/tokens` |
| `GET` | `/api/tokens` |
| `GET` | `/api/tokens/{id}` |
| `POST` | `/api/tokens/{id}/complete` |
| `POST` | `/api/tokens/{id}/cancel` |
| `POST` | `/api/v1/tokens/{id}/complete` |
| `POST` | `/api/v1/tokens/{id}/callback/{secret}` |
| `POST` | `/api/workspaces` |
| `GET` | `/api/workspaces` |
| `GET` | `/api/workspaces/{workspace_id}` |
| `PATCH` | `/api/workspaces/{workspace_id}` |
| `DELETE` | `/api/workspaces/{workspace_id}` |
| `POST` | `/api/workspaces/{workspace_id}/materialize` |
| `POST` | `/api/workspaces/{workspace_id}/connect` |
| `POST` | `/api/workspaces/{workspace_id}/stop` |
| `GET` | `/api/workspaces/{workspace_id}/files` |
| `GET` | `/api/workspaces/{workspace_id}/files/content` |
| `GET` | `/api/workspaces/{workspace_id}/files/stat` |
| `GET` | `/api/workspaces/{workspace_id}/versions` |
| `GET` | `/api/workspaces/{workspace_id}/versions/{version_id}` |
| `POST` | `/api/workspaces/{workspace_id}/execs` |
| `GET` | `/api/workspaces/{workspace_id}/execs` |
| `GET` | `/api/workspaces/{workspace_id}/execs/{exec_id}` |
| `POST` | `/api/workspaces/{workspace_id}/execs/{exec_id}/stdin` |
| `POST` | `/api/workspaces/{workspace_id}/execs/{exec_id}/stdin/close` |
| `GET` | `/api/workspaces/{workspace_id}/execs/{exec_id}/stdout` |
| `GET` | `/api/workspaces/{workspace_id}/execs/{exec_id}/stderr` |
| `POST` | `/api/workspaces/{workspace_id}/pty` |
| `GET` | `/api/workspaces/{workspace_id}/pty` |
| `GET` | `/api/workspaces/{workspace_id}/pty/{pty_id}` |
| `POST` | `/api/workspaces/{workspace_id}/pty/{pty_id}/input` |
| `GET` | `/api/workspaces/{workspace_id}/pty/{pty_id}/output` |
| `POST` | `/api/workspaces/{workspace_id}/pty/{pty_id}/resize` |
| `POST` | `/api/workspaces/{workspace_id}/pty/{pty_id}/close` |
| `GET` | `/api/schedules` |
| `GET` | `/api/schedules/{id}` |
| `POST` | `/api/tokens` |
| `GET` | `/api/tokens` |
| `GET` | `/api/tokens/{id}` |
| `POST` | `/api/tokens/{id}/complete` |
| `POST` | `/api/tokens/{id}/cancel` |
| `POST` | `/api/deployments` |
| `GET` | `/api/deployments/current` |
| `POST` | `/api/deployments/{id}/promote` |
| `POST` | `/api/secrets` |
| `GET` | `/api/secrets` |
| `GET` | `/api/secrets/{secret_id}` |
| `GET` | `/api/secrets/by-name/{name}` |
| `POST` | `/api/secrets/{secret_id}/rotate` |
| `POST` | `/api/secrets/by-name/{name}/rotate` |
| `POST` | `/api/secrets/{secret_id}/revoke` |
| `POST` | `/api/secrets/by-name/{name}/revoke` |

Auth routes include GitHub OAuth, magic links, device auth, logout, API keys, members, invitations, projects, and environments.

`POST /api/tokens` accepts an authenticated Environment-scoped caller with
`tokens.create`; omitted timeout defaults to 10 minutes. The SDK and REST API
support this external creation path, while CLI and Console creation are not v0
surfaces.

`POST /api/tokens/{id}/complete` accepts a Helmr API key or session bearer with `tokens.complete` permission for the token's project environment. Browser completion uses `POST /api/public/tokens/{id}/complete` with the token's scoped `public_access_token`; provider callbacks use the creation response's `/api/token-callbacks/{id}/{secret}` URL and do not use CORS. Token ID knowledge alone is not authorization. Retrying the same canonical completion replays; completing with different data returns `409 token_completion_conflict` and never overwrites the accepted result.

Actor input and output are fixed durable channels. They are addressed by the
Actor declaration plus exactly one Actor ID or key; callers never create or
name channel resources. Public browser grants for Actor channels are deferred.
The only public-access grant in v0 is `token.complete`.

Worker routes include registration, activation, drain/status, execution
lease/start/renew/release, log/event append, Actor input/output operations,
internal wait suspension, token creation, metadata updates, and checkpoint
ready/failed notifications.

`GET /api/runs/{id}/events` returns JSON pages by default and streams SSE when `follow=1` or `Accept: text/event-stream` is present. Page cursors and SSE `id` values are opaque run event cursors.

`GET /api/runs/{id}/logs` returns the latest stdout/stderr snapshot by default. The response `cursor` is a run-wide opaque log cursor. When `follow=1` or `Accept: text/event-stream` is present, the same route streams `run_log` SSE records after the supplied cursor. Pass the cursor as `Last-Event-ID` or `?cursor=CURSOR` to continue after chunks already received.

Workspace routes manage durable workspace records and live materializations.
`GET /api/workspaces/{workspace_id}/files/content?path=...` reads raw bytes
from a ready workspace version. `GET /api/workspaces/{workspace_id}/files`
lists direct children and `GET /api/workspaces/{workspace_id}/files/stat`
returns one file entry. File reads use `source=current` by default, where
`current` means the workspace's ready `current_version_id`. To read another
ready version in the same workspace, pass `source=version&version_id=...`.
`version_id` without `source=version` is rejected. `source=live` is reserved and
returns not implemented until live file reads are available. Version routes list
and retrieve ready versions only. File listing uses `limit` with a default of
200 and a maximum of 500. Version listing uses `limit` with a default of 100 and
a maximum of 200.

`POST /api/workspaces/{workspace_id}/execs` starts a write-capable command in
the workspace. `POST /api/workspaces/{workspace_id}/pty` starts an interactive
PTY. Exec stdout/stderr and PTY output routes return stored chunks by default
and stream SSE when `follow=1` or `Accept: text/event-stream` is present. Pass
the cursor as `Last-Event-ID` or `?cursor=N` to continue after chunks already
received.

Schedules are declared only with `schedules.task()` in source. Deployment
promotion reconciles them atomically. Authenticated Schedule routes are
read-only list/retrieve operations; timing or lifecycle changes require another
source Deployment promotion.

`POST /api/deployments` records the API version, CLI version, SDK version, bundle format version, and worker protocol version used to create the deployment. Deployment responses include those fields plus the immutable deployment `version`. Promotion is separate from creation; promoting a deployment moves the selected environment's current deployment pointer.
