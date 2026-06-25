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

The date is a fixed build constant, not the request date. The control plane echoes the effective version in `Helmr-API-Version`. Requests with an unsupported non-empty version return `400 Bad Request`; omitted versions currently default to the current version during pre-release development.

Client provenance headers are separate from the API contract:

| Header | Meaning |
| --- | --- |
| `Helmr-Client-Version` | Generic client build version. |
| `Helmr-CLI-Version` | CLI build version for CLI-originated requests. |
| `Helmr-SDK-Version` | SDK package version for SDK-originated requests. |

These provenance headers are recorded on deployments and runs where available. They are diagnostic metadata and should not be used as authorization or compatibility gates.

Common user/API-key routes:

| Method | Path |
| --- | --- |
| `POST` | `/api/tasks/{task_id}/start` |
| `POST` | `/api/tasks/{task_id}/start-and-wait` |
| `GET` | `/api/sessions` |
| `GET` | `/api/sessions/{id}` |
| `PATCH` | `/api/sessions/{id}` |
| `POST` | `/api/sessions/{id}/wait` |
| `POST` | `/api/sessions/{id}/close` |
| `POST` | `/api/sessions/{id}/cancel` |
| `GET` | `/api/sessions/{id}/runs` |
| `GET` | `/api/sessions/{id}/streams` |
| `POST` | `/api/sessions/{id}/inputs/{stream}` |
| `GET` | `/api/sessions/{id}/inputs/{stream}` |
| `POST` | `/api/sessions/{id}/outputs/{stream}` |
| `GET` | `/api/sessions/{id}/outputs/{stream}` |
| `GET` | `/api/sessions/{id}/outputs/{stream}/read` |
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
| `POST` | `/api/public-access-tokens` |
| `POST` | `/api/v1/tokens/{id}/complete` |
| `POST` | `/api/v1/tokens/{id}/callback/{secret}` |
| `POST` | `/api/v1/sessions/{id}/inputs/{stream}` |
| `GET` | `/api/v1/sessions/{id}/outputs/{stream}/read` |
| `POST` | `/api/workspaces` |
| `GET` | `/api/workspaces` |
| `GET` | `/api/workspaces/{workspace_id}` |
| `PATCH` | `/api/workspaces/{workspace_id}` |
| `DELETE` | `/api/workspaces/{workspace_id}` |
| `POST` | `/api/workspaces/{workspace_id}/materialize` |
| `POST` | `/api/workspaces/{workspace_id}/connect` |
| `POST` | `/api/workspaces/{workspace_id}/stop` |
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
| `POST` | `/api/schedules` |
| `GET` | `/api/schedules` |
| `GET` | `/api/schedules/{id}` |
| `PUT` | `/api/schedules/{id}` |
| `POST` | `/api/schedules/{id}/activate` |
| `POST` | `/api/schedules/{id}/deactivate` |
| `DELETE` | `/api/schedules/{id}` |
| `POST` | `/api/deployments` |
| `GET` | `/api/deployments/current` |
| `GET` | `/api/secrets` |
| `GET` | `/api/secrets/{name}` |
| `PUT` | `/api/secrets/{name}` |
| `DELETE` | `/api/secrets/{name}` |

Auth routes include GitHub OAuth, magic links, device auth, logout, API keys, members, invitations, projects, and environments.

`POST /api/tokens/{id}/complete` accepts a Helmr API key or session bearer with `tokens.complete` permission for the token's project environment. Browser completion uses `POST /api/v1/tokens/{id}/complete` with the token's scoped `public_access_token`; provider callbacks use `POST /api/v1/tokens/{id}/callback/{secret}` and do not use CORS. Token id knowledge is not authorization.

`POST /api/public-access-tokens` creates narrow browser capabilities bound to one stream scope. `session.input.send` tokens can call `POST /api/v1/sessions/{id}/inputs/{stream}`. `session.output.read` tokens can call `GET /api/v1/sessions/{id}/outputs/{stream}/read`. The public token's scope row, stream direction, and optional `correlation_id` are checked before the token is consumed.

Worker routes include registration, activation, drain/status, execution lease/start/renew/release, log/event append, internal wait suspension, token creation, stream output append, metadata updates, and checkpoint ready/failed notifications. Worker registration and status responses include `worker_group_id`. Worker run leases and worker run payloads include `attempt_number`; this is the task attempt number, not the queue dispatch attempt.

`GET /api/runs/{id}/events` returns JSON pages by default and streams SSE when `follow=1` or `Accept: text/event-stream` is present. The SSE `id` is the run event cursor.

`GET /api/runs/{id}/logs` returns the latest stdout/stderr snapshot by default. The response `cursor` is a run-wide log cursor. When `follow=1` or `Accept: text/event-stream` is present, the same route streams `run_log` SSE records after the supplied cursor. Pass the cursor as `Last-Event-ID` or `?cursor=N` to continue after chunks already received.

Workspace routes manage durable workspace records and live materializations.
`POST /api/workspaces/{workspace_id}/execs` starts a write-capable command in
the workspace. `POST /api/workspaces/{workspace_id}/pty` starts an interactive
PTY. Exec stdout/stderr and PTY output routes return stored chunks by default
and stream SSE when `follow=1` or `Accept: text/event-stream` is present. Pass
the cursor as `Last-Event-ID` or `?cursor=N` to continue after chunks already
received.

`POST /api/schedules` creates or replaces an imperative schedule for the selected project environment. The request body uses required `deduplication_key`, `task`, and `cron`, plus optional `external_id`, `timezone`, `active`, and schedule run `options`. `deduplication_key` is the stable public key for the project-level logical schedule and selected environment instance. Schedule requests do not accept arbitrary payload, secret bindings, or user-supplied idempotency options; scheduled runs receive Helmr-generated schedule metadata. `PUT /api/schedules/{id}` replaces the imperative schedule definition and selected environment instance settings and does not accept `deduplication_key`. Declarative schedules are synchronized from deployments and return `400 Bad Request` for imperative edit, activate, deactivate, or delete routes.

`POST /api/deployments` records the API version, CLI version, SDK version, bundle format version, and worker protocol version used to create the deployment. Deployment responses include those fields plus the immutable deployment `version`. Promotion is separate from creation; promoting a deployment moves the selected environment's current deployment pointer.
