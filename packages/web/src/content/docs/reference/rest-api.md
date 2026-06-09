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
| `POST` | `/api/runs` |
| `GET` | `/api/runs` |
| `GET` | `/api/runs/counts` |
| `GET` | `/api/runs/{id}` |
| `GET` | `/api/runs/{id}/events` |
| `GET` | `/api/runs/{id}/logs` |
| `POST` | `/api/waitpoints` |
| `POST` | `/api/waitpoints/{waitpointID}/respond` |
| `POST` | `/api/waitpoints/tokens` |
| `POST` | `/api/waitpoints/tokens/{id}/respond` |
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

Worker routes include registration, activation, drain/status, execution lease/start/renew/release, log/event append, waitpoints, and checkpoint ready/failed notifications. Worker registration and status responses include `worker_group_id`. Worker run leases and worker run payloads include `attempt_number`; this is the task attempt number, not the queue dispatch attempt.

`GET /api/runs/{id}/events` returns JSON pages by default and streams SSE when `follow=1` or `Accept: text/event-stream` is present.

`POST /api/schedules` creates or replaces an imperative schedule for the selected project environment. The request body uses required `deduplication_key`, `task`, and `cron`, plus optional `external_id`, `timezone`, `active`, and schedule run `options`. `deduplication_key` is the stable public key for the project-level logical schedule and selected environment instance. Schedule requests do not accept arbitrary payload, secret bindings, or user-supplied idempotency options; scheduled runs receive Helmr-generated schedule metadata. `PUT /api/schedules/{id}` replaces the imperative schedule definition and selected environment instance settings and does not accept `deduplication_key`. Declarative schedules are synchronized from deployments and return `400 Bad Request` for imperative edit, activate, deactivate, or delete routes.

`POST /api/deployments` records the API version, CLI version, SDK version, bundle format version, and worker protocol version used to create the deployment. Deployment responses include those fields plus the immutable deployment `version`. Promotion is separate from creation; promoting a deployment moves the selected environment's current deployment pointer.
