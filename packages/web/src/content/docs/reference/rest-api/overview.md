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
| Sandboxes | `GET /v1/sandboxes`, `GET /v1/sandboxes/{id}`, `POST .../{id}/computers` |
| Commands | `GET /v1/commands/{id}`, `GET /v1/commands/{id}/logs` |
| Computers | `GET /v1/computers`, `GET`/`DELETE /v1/computers/{id}`, `POST .../exec` |
| Runs | `GET /v1/runs`, `GET /v1/runs/{id}`, `GET .../events`, `GET .../logs`, `POST .../cancel` |
| Deployments | `GET`/`POST /v1/deployments`, `GET /v1/deployments/current`, `GET /v1/deployments/{id}`, `GET .../events`, `POST .../promote` |
| Schedules | `GET /v1/schedules`, `GET /v1/schedules/{id}` |
| Secrets | `GET`/`POST /v1/secrets`, `GET /v1/secrets/{id}`, `POST .../rotate`, `POST .../revoke` |
| Tokens | `GET`/`POST /v1/tokens`, `GET /v1/tokens/{id}`, `POST .../complete`, `POST .../cancel` |

JSON wire fields use snake case. Collection envelopes use the plural resource
name and optionally `next_cursor`; item routes return the resource object.
Computer exec returns a Command admission receipt. Command log pages contain
base64-encoded byte chunks or explicit gap records, scoped reconnect cursors, and
an `output_state` observation (`open`, `closed`, or `unavailable`). Run
logs and events are finite JSON pages.

Console/session management and public callbacks use `/api`, Admin uses
`/admin/api/v1`, Capacity uses `/capacity/v1`, and the Worker protocol uses
`/worker/v1`. None are aliases for the `/v1` Developer API. Self-hosted
operators can integrate trusted scaling automation through the separate
[Capacity deployment protocol](/docs/self-hosting/capacity-scaling).
