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

| Resource | Method | Path |
| --- | --- | --- |
| Deployments | `GET` | `/v1/deployments` |
| | `GET` | `/v1/deployments/current` |
| | `GET` | `/v1/deployments/{deploymentID}` |
| | `GET` | `/v1/deployments/{deploymentID}/events` |
| | `POST` | `/v1/deployments/{deploymentID}/promote` |
| Deployment bundles | `POST` | `/v1/deployment-bundles/upload-plan` |
| | `POST` | `/v1/deployment-bundles/finalize` |
| Agent definitions | `GET` | `/v1/agent-definitions` |
| | `GET` | `/v1/agent-definitions/{agentDefinitionID}` |
| Agents | `POST` | `/v1/agents/{agentName}/start` |
| Sessions | `GET` | `/v1/sessions` |
| | `GET` | `/v1/sessions/{sessionID}` |
| | `GET` | `/v1/sessions/{sessionID}/events` |
| | `POST` | `/v1/sessions/{sessionID}/enqueue` |
| | `POST` | `/v1/sessions/{sessionID}/send` |
| | `POST` | `/v1/sessions/{sessionID}/interrupt` |
| | `POST` | `/v1/sessions/{sessionID}/resume` |
| | `POST` | `/v1/sessions/{sessionID}/close` |
| | `POST` | `/v1/sessions/{sessionID}/cancel` |
| Turns | `GET` | `/v1/sessions/{sessionID}/turns` |
| | `GET` | `/v1/sessions/{sessionID}/turns/{turnID}` |
| | `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/messages` |
| Questions | `GET` | `/v1/sessions/{sessionID}/turns/{turnID}/asks` |
| | `GET` | `/v1/sessions/{sessionID}/turns/{turnID}/asks/{askID}` |
| | `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/asks/{askID}/respond` |
| Schedules | `GET` | `/v1/schedules` |
| | `GET` | `/v1/schedules/{scheduleID}` |
| Template definitions | `GET` | `/v1/template-definitions` |
| | `GET` | `/v1/template-definitions/{templateDefinitionID}` |
| | `POST` | `/v1/template-definitions/{templateDefinitionID}/computers` |
| Computers | `GET` | `/v1/computers` |
| | `GET` | `/v1/computers/{computerID}` |
| | `PATCH` | `/v1/computers/{computerID}` |
| | `DELETE` | `/v1/computers/{computerID}` |
| | `GET` | `/v1/computers/{computerID}/members` |
| | `POST` | `/v1/computers/{computerID}/exec` |
| Commands | `GET` | `/v1/commands/{commandID}` |
| | `GET` | `/v1/commands/{commandID}/logs` |
| | `POST` | `/v1/commands/{commandID}/cancel` |
| Secrets | `GET` | `/v1/secrets` |
| | `POST` | `/v1/secrets` |
| | `GET` | `/v1/secrets/{secretID}` |
| | `POST` | `/v1/secrets/{secretID}/rotate` |
| | `POST` | `/v1/secrets/{secretID}/revoke` |

`{agentName}`, `{agentDefinitionID}` and `{templateDefinitionID}` are
definition IDs declared in code. All other path IDs are server-assigned UUIDs.

`PATCH /v1/computers/{computerID}` changes Computer retention. Its body is
`{ "delete_after_unused_ms": <positive integer> | null }`, where `null` means
never delete. It requires the `computers:update` permission.

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
- Start returns a Session and Turn admission; exec returns a Command
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
