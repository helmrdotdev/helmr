---
title: REST API
description: Developer API routes, conventions and related route families.
sidebarLabel: Overview
---

# REST API

The Developer API is rooted at `/v1` on the control plane origin. Every route
authenticates an Environment-bound API key with `Authorization: Bearer <key>`;
see [Authentication](/docs/reference/api/authentication). The TypeScript
[`HelmrClient`](/docs/reference/sdk/client) wraps these routes.

## Routes

| Method | Path |
| --- | --- |
| `DELETE` | `/v1/computers/{computerID}` |
| `GET` | `/v1/agents` |
| `GET` | `/v1/agents/{agent}` |
| `GET` | `/v1/commands/{commandID}` |
| `GET` | `/v1/commands/{commandID}/logs` |
| `GET` | `/v1/computers` |
| `GET` | `/v1/computers/{computerID}` |
| `GET` | `/v1/computers/{computerID}/sessions` |
| `GET` | `/v1/computers/{computerID}/commands` |
| `GET` | `/v1/deployments` |
| `GET` | `/v1/deployments/current` |
| `GET` | `/v1/deployments/{deploymentID}` |
| `GET` | `/v1/deployments/{deploymentID}/events` |
| `GET` | `/v1/schedules` |
| `GET` | `/v1/schedules/{scheduleID}` |
| `GET` | `/v1/secrets` |
| `GET` | `/v1/secrets/{secretID}` |
| `GET` | `/v1/sessions` |
| `GET` | `/v1/sessions/{sessionID}` |
| `GET` | `/v1/sessions/{sessionID}/events` |
| `GET` | `/v1/sessions/{sessionID}/turns` |
| `GET` | `/v1/sessions/{sessionID}/turns/{turnID}` |
| `GET` | `/v1/sessions/{sessionID}/turns/{turnID}/asks` |
| `GET` | `/v1/sessions/{sessionID}/turns/{turnID}/asks/{askID}` |
| `GET` | `/v1/templates` |
| `GET` | `/v1/templates/{template}` |
| `PATCH` | `/v1/computers/{computerID}` |
| `POST` | `/v1/commands/{commandID}/cancel` |
| `POST` | `/v1/computers` |
| `POST` | `/v1/computers/{computerID}/exec` |
| `POST` | `/v1/deployment-bundles/finalize` |
| `POST` | `/v1/deployment-bundles/upload-plan` |
| `POST` | `/v1/deployments/{deploymentID}/promote` |
| `POST` | `/v1/secrets` |
| `POST` | `/v1/secrets/{secretID}/revoke` |
| `POST` | `/v1/secrets/{secretID}/rotate` |
| `POST` | `/v1/sessions` |
| `POST` | `/v1/sessions/{sessionID}/cancel` |
| `POST` | `/v1/sessions/{sessionID}/close` |
| `POST` | `/v1/sessions/{sessionID}/enqueue` |
| `POST` | `/v1/sessions/{sessionID}/interrupt` |
| `POST` | `/v1/sessions/{sessionID}/resume` |
| `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/asks/{askID}/respond` |
| `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/interrupt` |
| `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/messages` |

`{agent}` and `{template}` are identifiers declared in code. Other resource path
IDs are server-assigned UUIDs. Agent/Template list and get resolve the current
Deployment unless `deployment_id` selects an exact Deployment. Responses include
that resolved `deployment_id`; list envelopes are `agents` and `templates`.

| Resource | Identifier and inspection fields |
| --- | --- |
| Agent | `agent`, default `template`, `deployment_id` |
| Template | `template`, `resources`, `deployment_id` |
| Session | `agent`, `template` (the pinned Agent default), `deployment_id`, optional `computer_id` |
| Computer | `template`, `deployment_id` (its actual creation origin), optional instance `key` |
| Schedule | `agent`, `deployment_id` |

Template resources use `milliCpu`, `memoryMiB` and optional `diskMiB`. The local
`agent({ id })` / `template({ id })` declaration value is called `agent` /
`template` in this API. It is never guessed to be a UUID, even if it looks like one.
Session creation uses `{"agent":"support"}`; Computer creation uses
`{"template":"browser"}`. Both use current for new work.

A missing current Deployment returns `no_current_deployment`. An unknown selected
Deployment returns `deployment_not_found`. A missing Agent/Template in a resolved,
authorized Deployment returns `not_found` with `error.details.deployment_id`;
it does not fall back to another Deployment or claim the stable identity was deleted.

Session and Schedule lists accept the declared `agent` identifier.
`agent` resolves stable identity independently of current membership, so retained
history remains searchable after an Agent is removed from current.

Computer `/sessions` and `/commands` return current usage, including work whose
process cleanup is pending. They are not history endpoints. Entries contain `id`,
`usage_state` and `created_at`; usage state does not replace resource lifecycle
status. The page envelopes are `sessions` or `commands`, with `next_cursor` when
needed. Each collection has its own cursor.

`PATCH /v1/computers/{computerID}` changes retention with
`{"delete_after_unused_ms": <positive integer> | null}`; `null` means never delete.
It requires `computers:update`.

## Conventions

- Request and response bodies are JSON. Most fields use snake case. Secret
  bindings use `secretId`, `env`, `file` and `allowedOrigins`, the same names
  as the SDK.
- Input and messages are arrays of text parts. Output and responses are
  Content arrays. See [Agent](/docs/reference/sdk/agent#content-rules).
- Retention fields are milliseconds: `close_after_idle_ms` and
  `delete_after_unused_ms` take a positive integer or `null` for never. Omit
  them to inherit the definition.
- Mutations accept an `idempotency_key`; question responses use
  `response_id`. Reuse the same value and body to retry an uncertain request.
  See [Errors](/docs/reference/api/errors).
- Session creation returns a Session and an optional initial Turn admission; exec returns a Command
  admission. Neither confirms completion.
- Collections use opaque cursors; Session events use integer sequences. See
  [Pagination](/docs/reference/api/pagination).
- Command logs contain base64-encoded byte chunks or explicit gaps, with
  reconnect cursors and an `output_state`.
- Session events contain authored content and lifecycle facts. Process
  stdout and stderr are not exposed. See [Session events](/docs/reference/events).

## Other route families

| Prefix | Purpose |
| --- | --- |
| `/api` | Management used by the console and by CLI commands with a saved login. |
| `/admin/api/v1` | Administration. |
| `/capacity/v1` | Capacity scaling for self-hosted deployments. See [Capacity](/docs/self-hosting/capacity). |
| `/worker/v1` | Worker control. |
| `/integrations/slack/apps/{registrationID}/events`, `/interactions` | Slack app callbacks. |
| `/healthz`, `/readyz` | Liveness and readiness. |

These families have their own authentication and are not alternatives to the
Developer API.
