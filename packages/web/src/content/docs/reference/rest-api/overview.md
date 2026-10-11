---
title: REST API
description: Environment-scoped developer routes for Agent execution and retained resources.
---

# REST API

The Developer API is rooted at `/v1` and authenticates an Environment-bound API
key. The following resource families are public; worker and administration routes
have separate authority.

| Resource | Methods and paths |
| --- | --- |
| Agents | `GET /v1/agents`, `GET /v1/agents/{name}`, `POST /v1/agents/{name}/start` |
| Sessions | `GET /v1/sessions`, `GET /v1/sessions/{id}`, `POST .../enqueue`, `POST .../send`, `GET .../events` |
| Session controls | `POST /v1/sessions/{id}/interrupt`, `POST .../resume`, `POST .../close`, `POST .../cancel` |
| Turns | `GET /v1/sessions/{id}/turns`, `GET .../turns/{turnID}`, `POST .../turns/{turnID}/messages` |
| Questions | `GET /v1/sessions/{id}/turns/{turnID}/asks`, `GET .../asks/{askID}`, `POST .../asks/{askID}/respond` |
| Computer definitions | `GET /v1/computer-definitions`, `GET /v1/computer-definitions/{id}`, `POST .../{id}/computers` |
| Computers | `GET /v1/computers`, `GET`/`DELETE /v1/computers/{id}`, `GET .../members`, `POST .../exec` |
| Commands | `GET /v1/commands/{id}`, `GET .../logs`, `POST .../cancel` |
| Deployments | `GET /v1/deployments`, `GET /v1/deployments/current`, `GET /v1/deployments/{id}`, `GET .../events`, `POST .../promote` |
| Bundle upload | `POST /v1/deployment-bundles/upload-plan`, `POST /v1/deployment-bundles/finalize` |
| Schedules | `GET /v1/schedules`, `GET /v1/schedules/{id}` |
| Secrets | `GET`/`POST /v1/secrets`, `GET /v1/secrets/{id}`, `POST .../rotate`, `POST .../revoke` |
| Slack destinations | `GET /v1/slack/channels`, `GET /v1/slack/channels/{id}` |

Agent and Computer definition path IDs are authored names. Actual resource IDs
are server identities. Start returns Session and Turn admission; Computer exec
returns a Command admission. Neither receipt establishes execution completion.

Most wire fields use snake case. Secret bindings use `secretId` and
`allowedOrigins`. Collection envelopes name their resources and use opaque
`next_cursor` where applicable. Session events use durable integer sequences
with `next_after`, `has_more` and `retained_after` instead.

Command logs contain base64-encoded byte chunks or explicit gaps, reconnect
cursors and an `output_state` observation. Session events contain authored content
and lifecycle facts; process stdout/stderr and preparation diagnostics are internal.

Console management uses `/api`, Admin uses `/admin/api/v1`, Capacity uses
`/capacity/v1` and Worker control uses `/worker/v1`. These are not aliases for
Developer API authentication. See [Capacity scaling](/docs/self-hosting/capacity-scaling)
for the trusted deployment protocol.
