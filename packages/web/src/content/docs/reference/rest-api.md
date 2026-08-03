---
title: REST API
description: Control-plane API routes used by the CLI, SDK, web UI, and workers.
section: Reference
sidebarLabel: REST API
order: 940
---

# REST API

The Control Plane serves JSON APIs under `/api`. Authenticated user/API-key requests use `Authorization: Bearer TOKEN`. Worker requests use worker bearer tokens minted by `/api/worker/auth/token`.

## API version header

User, API-key, console, CLI, SDK, and worker API requests use a date-pinned API contract header:

```http
Helmr-API-Version: 2026-06-06
```

The date is a fixed build constant, not the request date. The Control Plane echoes the effective version in `Helmr-API-Version`. Requests with an unsupported non-empty version return `400 Bad Request`; omitted versions currently default to the current version during pre-release development. Header values are exact and are not trimmed.

Common user/API-key routes:

| Method | Path |
| --- | --- |
| `POST` | `/api/tasks/{taskDeclaredID}/start` |
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
| `POST` | `/api/workspaces/{workspaceDeclaredID}/create` |
| `GET` | `/api/workspaces/by-key/{workspaceDeclaredID}?key=...` |
| `GET` | `/api/workspaces/{workspace_id}` |
| `POST` | `/api/workspaces/{workspace_id}/delete` |
| `GET` | `/api/workspaces/{workspace_id}/files` |
| `GET` | `/api/workspaces/{workspace_id}/files/content` |
| `GET` | `/api/workspaces/{workspace_id}/files/stat` |
| `POST` | `/api/workspaces/{workspace_id}/exec` |
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

`GET /api/runs/{id}/events` returns one finite JSON page. Its cursor is opaque.

`GET /api/runs/{id}/logs` returns one finite page of stdout, stderr, and
structured log records. The response cursor is opaque. Clients poll from the
next cursor when they need to follow progress.

Workspace routes expose create/ref/retrieve/delete, committed file reads, and
one bounded basic exec. `GET /api/workspaces/{workspace_id}/files/content`
reads raw bytes; the list and stat routes return committed file metadata.

`POST /api/workspaces/{workspace_id}/exec` executes one write-capable command
and returns its bounded terminal stdout, stderr, and exit code. Process handles,
PTYs, materialization controls, and public Workspace-version management are not
v0 routes.

Schedules are declared only with `schedules.task()` in source. Deployment
promotion reconciles them atomically. Authenticated Schedule routes are
read-only list/retrieve operations; timing or lifecycle changes require another
source Deployment promotion.

`POST /api/deployments` records the API and worker-protocol versions used to create the deployment. Deployment responses include those fields plus the immutable deployment `version`. Promotion is separate from creation; promoting a deployment moves the selected environment's current deployment pointer.
