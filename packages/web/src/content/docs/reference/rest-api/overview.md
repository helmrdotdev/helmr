---
title: REST API
description: Verified public Developer API routes under /v1.
sidebarLabel: Overview
---

# REST API

The public Developer API is rooted at `/v1` and uses an Environment-bound API
key. The table below is the public subset asserted by the Control Plane route
tests; it is not an OpenAPI specification or a claim that every internal route
is public.

| Resource | Methods and paths |
| --- | --- |
| Tasks | `GET /v1/tasks`, `GET /v1/tasks/{id}`, `POST /v1/tasks/{declaredID}/start` |
| Actors | `GET /v1/actors`, `GET /v1/actors/{id}`, `POST /v1/actors/{declaredID}/start` |
| Sessions | `GET /v1/sessions`, `GET /v1/sessions/{id}`, `POST .../inputs`, `GET .../outputs`, `POST .../close` |
| Sandboxes | `GET /v1/sandboxes`, `GET /v1/sandboxes/{id}`, `POST .../{id}/workspaces` |
| Workspaces | `GET /v1/workspaces`, `GET`/`DELETE /v1/workspaces/{id}`, `GET .../files`, `GET .../files/content`, `GET .../files/stat`, `POST .../exec`, `GET .../exec/{process_id}` |
| Runs | `GET /v1/runs`, `GET /v1/runs/{id}`, `GET .../events`, `GET .../logs`, `POST .../cancel` |
| Deployments | `GET`/`POST /v1/deployments`, `GET /v1/deployments/current`, `GET /v1/deployments/{id}`, `GET .../events`, `POST .../promote` |
| Schedules | `GET /v1/schedules`, `GET /v1/schedules/{id}` |
| Secrets | `GET`/`POST /v1/secrets`, `GET /v1/secrets/{id}`, `POST .../rotate`, `POST .../revoke` |
| Tokens | `GET`/`POST /v1/tokens`, `GET /v1/tokens/{id}`, `POST .../complete`, `POST .../cancel` |

JSON wire fields use snake case. Collection envelopes use the plural resource
name and optionally `next_cursor`; item routes return the resource object.
Workspace file content is a JSON object whose `data_base64` field contains the
file bytes. The SDK and the CLI's default output decode that value. Run logs
and events are finite JSON pages.

Console/session management, public callback, worker, capacity, and Admin APIs
use other prefixes and are not aliases for the `/v1` Developer API. Self-hosted
operators can integrate trusted scaling automation through the separate
[Capacity deployment protocol](/docs/self-hosting/capacity-scaling/).
