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
| `GET` | `/api/sessions/{id}/workspace` |
| `GET` | `/api/sessions/{id}/channels/{channel}/inputs` |
| `POST` | `/api/sessions/{id}/channels/{channel}/inputs` |
| `GET` | `/api/sessions/{id}/channels/{channel}/outputs` |
| `GET` | `/api/sessions/{id}/channels/{channel}/outputs/stream` |
| `GET` | `/api/runs` |
| `GET` | `/api/runs/counts` |
| `GET` | `/api/runs/{id}` |
| `GET` | `/api/runs/{id}/events` |
| `GET` | `/api/runs/{id}/logs` |
| `GET` | `/api/runs/{id}/waitpoints` |
| `POST` | `/api/waitpoints/tokens` |
| `GET` | `/api/waitpoints/tokens` |
| `GET` | `/api/waitpoints/tokens/{id}` |
| `POST` | `/api/waitpoints/tokens/{id}/complete` |
| `POST` | `/api/waitpoints/tokens/{id}/callback/{secret}` |
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

`POST /api/waitpoints/tokens/{id}/complete` accepts a Helmr API key or session bearer with `waitpoint_tokens.complete` permission for the token's project environment, or the waitpoint token's `publicAccessToken` bearer. Public tokens use the `hlmr_wpt_` prefix and authorize only completion of their own token. The callback route is a pre-signed server-to-server alternative; callback secrets use the `hlmr_wpc_` prefix and are embedded in the callback URL path. Creation responses include `public_access_token` and `callback_url`; retrieve and list responses do not return completion secrets.

Worker routes include registration, activation, drain/status, execution lease/start/renew/release, log/event append, waitpoints, waitpoint token creation, channel output append, metadata updates, and checkpoint ready/failed notifications. Worker registration and status responses include `worker_group_id`. Worker run leases and worker run payloads include `attempt_number`; this is the task attempt number, not the queue dispatch attempt.

`GET /api/runs/{id}/events` returns JSON pages by default and streams SSE when `follow=1` or `Accept: text/event-stream` is present. The SSE `id` is the run event cursor.

`GET /api/runs/{id}/logs` returns the latest stdout/stderr snapshot by default. The response `cursor` is a run-wide log cursor. When `follow=1` or `Accept: text/event-stream` is present, the same route streams `run_log` SSE records after the supplied cursor. Pass the cursor as `Last-Event-ID` or `?cursor=N` to continue after chunks already received.

`POST /api/schedules` creates or replaces an imperative schedule for the selected project environment. The request body uses required `deduplication_key`, `task`, and `cron`, plus optional `external_id`, `timezone`, `active`, and schedule run `options`. `deduplication_key` is the stable public key for the project-level logical schedule and selected environment instance. Schedule requests do not accept arbitrary payload, secret bindings, or user-supplied idempotency options; scheduled runs receive Helmr-generated schedule metadata. `PUT /api/schedules/{id}` replaces the imperative schedule definition and selected environment instance settings and does not accept `deduplication_key`. Declarative schedules are synchronized from deployments and return `400 Bad Request` for imperative edit, activate, deactivate, or delete routes.

`POST /api/deployments` records the API version, CLI version, SDK version, bundle format version, and worker protocol version used to create the deployment. Deployment responses include those fields plus the immutable deployment `version`. Promotion is separate from creation; promoting a deployment moves the selected environment's current deployment pointer.
