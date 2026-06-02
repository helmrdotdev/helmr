---
title: REST API
description: Control-plane API routes used by the CLI, SDK, web UI, and workers.
section: Reference
sidebarLabel: REST API
order: 940
---

# REST API

The control plane serves JSON APIs under `/api`. Authenticated user/API-key requests use `Authorization: Bearer TOKEN`. Worker requests use worker bearer tokens minted by `/api/worker/auth/token`.

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
| `PUT` | `/api/secrets/{name}` |

Auth routes include GitHub OAuth, magic links, device auth, logout, API keys, members, invitations, projects, environments, and GitHub repository setup.

Worker routes include registration, activation, drain/status, execution lease/start/renew/release, log/event append, waitpoints, and checkpoint ready/failed notifications.

`GET /api/runs/{id}/events` returns JSON pages by default and streams SSE when `follow=1` or `Accept: text/event-stream` is present.

`POST /api/schedules` creates an imperative schedule. The request body uses `deduplication_key`, `external_id`, `task`, `cron`, `timezone`, `active`, `workspace`, `secret_bindings`, and optional schedule run `options`. Schedule requests do not accept arbitrary payload or user-supplied idempotency options; scheduled runs receive Helmr-generated schedule metadata. `PUT /api/schedules/{id}` replaces the imperative schedule definition except for `deduplication_key`. Declarative schedules are synchronized from deployments and return `400 Bad Request` for imperative edit, activate, deactivate, or delete routes.
