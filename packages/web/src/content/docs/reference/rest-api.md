---
title: REST API
description: Control-plane API routes used by the CLI, SDK, web UI, and workers.
section: Reference
sidebarLabel: REST API
order: 940
---

# REST API

The public Developer API is served under `/v1` and uses an Environment-bound API key. Session-authenticated Console and CLI management calls use `/api/projects/{projectID}/environments/{environmentID}`. Worker requests use the separately versioned `/api/worker/v0` protocol. Deployment capacity automation uses `/api/capacity/v0`.

The `/v1` path selects the public API resource family. Helmr does not use an API-version request header.

Common Developer API routes:

| Method | Path |
| --- | --- |
| `GET` | `/v1/tasks` |
| `GET` | `/v1/tasks/{taskID}` |
| `POST` | `/v1/tasks/{taskID}/start` |
| `GET` | `/v1/actors` |
| `GET` | `/v1/actors/{actorID}` |
| `POST` | `/v1/actors/{actorID}/start` |
| `GET` | `/v1/sandboxes` |
| `GET` | `/v1/sandboxes/{sandboxID}` |
| `POST` | `/v1/sandboxes/{sandboxID}/workspaces` |
| `GET` | `/v1/sessions` |
| `GET` | `/v1/sessions/{sessionID}` |
| `POST` | `/v1/sessions/{sessionID}/inputs` |
| `GET` | `/v1/sessions/{sessionID}/outputs` |
| `POST` | `/v1/sessions/{sessionID}/close` |
| `GET` | `/v1/runs` |
| `GET` | `/v1/runs/{runID}` |
| `GET` | `/v1/runs/{runID}/events` |
| `GET` | `/v1/runs/{runID}/logs` |
| `POST` | `/v1/runs/{runID}/cancel` |
| `GET` | `/v1/workspaces` |
| `GET` | `/v1/workspaces/{workspaceID}` |
| `DELETE` | `/v1/workspaces/{workspaceID}` |
| `GET` | `/v1/workspaces/{workspaceID}/files` |
| `GET` | `/v1/workspaces/{workspaceID}/files/content` |
| `GET` | `/v1/workspaces/{workspaceID}/files/stat` |
| `POST` | `/v1/workspaces/{workspaceID}/exec` |
| `GET` | `/v1/secrets` |
| `POST` | `/v1/secrets` |
| `GET` | `/v1/secrets/{secretID}` |
| `POST` | `/v1/secrets/{secretID}/rotate` |
| `POST` | `/v1/secrets/{secretID}/revoke` |
| `POST` | `/v1/tokens` |
| `GET` | `/v1/tokens` |
| `GET` | `/v1/tokens/{tokenID}` |
| `POST` | `/v1/tokens/{tokenID}/complete` |
| `POST` | `/v1/tokens/{tokenID}/cancel` |

Auth routes include GitHub OAuth, magic links, device auth, logout, API keys, members, invitations, projects, and environments.

`POST /api/tokens` accepts an authenticated Environment-scoped caller with
`tokens.create`; omitted timeout defaults to 10 minutes. The SDK and REST API
support this external creation path, while CLI and Console creation are not v0
surfaces.

`POST /api/tokens/{id}/complete` accepts a Helmr API key or session bearer with `tokens.complete` permission for the token's project environment. Browser completion uses `POST /api/public/tokens/{id}/complete` with the token's scoped `public_access_token`; provider callbacks use the creation response's `/api/token-callbacks/{id}/{secret}` URL and do not use CORS. Token ID knowledge alone is not authorization. Retrying the same canonical completion replays; completing with different data returns `409 token_completion_conflict` and never overwrites the accepted result.

Actor is a source definition. Starting one creates a server-identified Session
and its initial Run. Subsequent input, output, close, and retrieve operations use
the Session UUID. `GET /v1/sessions?actor_id={actorID}&key={key}` resolves the
optional caller-selected Session key without introducing a `by-key` route.

Worker routes are grouped below `/api/worker/v0/enrollment`,
`/api/worker/v0/instance`, `/api/worker/v0/build`, and
`/api/worker/v0/run`. They are an internal protocol, not Developer API aliases.

`GET /v1/runs/{runID}/events` returns one finite JSON page. Its cursor is opaque.

`GET /v1/runs/{runID}/logs` returns one finite page of stdout, stderr, and
structured log records. The response cursor is opaque. Clients poll from the
next cursor when they need to follow progress.

Sandbox is a source definition. Creating from it returns a server-identified
Workspace. Workspace routes expose retrieve/delete, committed file reads, and
one bounded basic exec. `GET /v1/workspaces/{workspaceID}/files/content`
reads raw bytes; the list and stat routes return committed file metadata.

`POST /v1/workspaces/{workspaceID}/exec` executes one write-capable command
and returns its bounded terminal stdout, stderr, and exit code. Process handles,
PTYs, materialization controls, and public Workspace-version management are not
v0 routes.

Schedules are declared only with `schedules.task()` in source. Deployment
promotion reconciles them atomically. Authenticated Schedule routes are
read-only list/retrieve operations; timing or lifecycle changes require another
source Deployment promotion.

All externally observable lifecycle fields are named `status`. `state` is
reserved for exact internal state-machine authority. List responses nest the
collection under its plural resource name, such as `{ "sessions": [...] }` or
`{ "workspaces": [...] }`; item routes return the resource object directly.

`POST /v1/deployments` records the API and worker-protocol versions used to create the deployment. Deployment responses include those fields plus the immutable deployment `version`. Promotion is separate from creation; promoting a deployment moves the selected environment's current deployment pointer.
