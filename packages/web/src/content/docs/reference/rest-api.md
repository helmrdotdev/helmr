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
| `POST` | `/api/runs/{id}/waitpoints/{waitpointID}/complete` |
| `POST` | `/api/waitpoints/tokens` |
| `POST` | `/api/waitpoints/tokens/{id}/complete` |
| `POST` | `/api/deployments` |
| `GET` | `/api/deployments/current` |
| `GET` | `/api/secrets` |
| `PUT` | `/api/secrets/{name}` |

Auth routes include GitHub OAuth, magic links, device auth, logout, API keys, members, invitations, projects, environments, and GitHub repository setup.

Worker routes include registration, activation, drain/status, execution lease/start/renew/release, log/event append, waitpoints, and checkpoint ready/failed notifications.

`GET /api/runs/{id}/events` returns JSON pages by default and streams SSE when `follow=1` or `Accept: text/event-stream` is present.
