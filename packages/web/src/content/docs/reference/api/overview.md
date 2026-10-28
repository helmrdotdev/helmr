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

Human-session [permission management](/docs/reference/api/permissions) uses the
`/api` Roles, Groups, Role Grants and member-permissions routes. These routes are
shared by the Console and CLI and do not accept workload API keys.

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
| `GET` | `/v1/triggers` |
| `GET` | `/v1/triggers/{triggerID}` |
| `GET` | `/v1/triggers/{triggerID}/occurrences` |
| `GET` | `/v1/secrets` |
| `GET` | `/v1/secrets/{secret}` |
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
| `PUT` | `/v1/deployment-bundles/{bundleDigest}/objects/{objectDigest}` |
| `POST` | `/v1/deployments/{deploymentID}/promote` |
| `POST` | `/v1/secrets` |
| `POST` | `/v1/secrets/{secret}/revoke` |
| `POST` | `/v1/secrets/{secret}/rotate` |
| `POST` | `/v1/sessions` |
| `POST` | `/v1/sessions/{sessionID}/cancel` |
| `POST` | `/v1/sessions/{sessionID}/close` |
| `POST` | `/v1/sessions/{sessionID}/enqueue` |
| `POST` | `/v1/sessions/{sessionID}/interrupt` |
| `POST` | `/v1/sessions/{sessionID}/resume` |
| `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/asks/{askID}/answer` |
| `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/interrupt` |
| `POST` | `/v1/sessions/{sessionID}/turns/{turnID}/messages` |

`{agent}` and `{template}` are identifiers declared in code. `{secret}` is the
Environment-scoped ID chosen when creating the Secret, such as `GITHUB_TOKEN`.
Project and Environment selectors use the readable `project` and `environment`
IDs shown in the Console. Instance resource IDs are server-assigned UUIDs. Agent/Template list and get resolve the current
Deployment unless `deployment_id` selects an exact Deployment. Responses include
that resolved `deployment_id`; list envelopes are `agents` and `templates`.

| Resource | Identifier and inspection fields |
| --- | --- |
| Agent | `agent`, default `template`, `deployment_id` |
| Template | `template`, `resources`, `deployment_id` |
| Session | `agent`, `template` (the pinned Agent default), `deployment_id`, optional `computer_id` |
| Computer | `template`, `deployment_id` (its actual creation origin), optional instance `key` |
| Trigger | `agent`, `deployment_id` |

Template resources use `milliCpu`, `memoryMiB` and optional `diskMiB`. The local
`agent({ id })` / `template({ id })` declaration value is called `agent` /
`template` in this API. It is never guessed to be a UUID, even if it looks like one.
Session creation uses `{"agent":"support"}`; Computer creation uses
`{"template":"browser"}`. Both use current for new work.

A missing current Deployment returns `no_current_deployment`. An unknown selected
Deployment returns `deployment_not_found`. A missing Agent/Template in a resolved,
authorized Deployment returns `not_found` with `error.details.deployment_id`;
it does not fall back to another Deployment or claim the stable identity was deleted.

Session and Trigger lists accept the declared `agent` identifier.
`agent` resolves stable identity independently of current membership, so retained
history remains searchable after an Agent is removed from current or deleted;
Trigger history requires `view=all`.

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
  bindings use `secret`, `env`, `file` and `allowedOrigins`, the same names
  as the SDK.
- Inputs are JSON values validated by the Agent's input schema. Agents without
  an input schema accept arrays of text parts, as do Turn messages. Output and
  responses are Content arrays. See [Agent](/docs/reference/sdk/agent#content-rules).
- Retention fields are milliseconds: `close_after_idle_ms` and
  `delete_after_unused_ms` take a positive integer or `null` for never. Omit
  them to inherit the definition.
- Mutations accept an `idempotency_key`; question answers use
  `answer_id`. Reuse the same value and body to retry an uncertain request.
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

## Trigger activations and occurrences

`GET /v1/triggers` accepts `agent`, logical trigger `key`, `view`, `limit` and
`cursor`. `view=current` is the default; `view=all` includes retained historical
activations. Keep the view and filters unchanged when using a cursor. A Trigger
ID identifies one activation, not the logical key shared across promotions.

`GET /v1/triggers/{triggerID}/occurrences` requires Session read access to the
Agent. It accepts `limit` (1–100, default 50) and `cursor`, and returns an
`occurrences` array with optional `next_cursor`. Each outcome has `scheduled_at`,
`evaluated_at`, `disposition` (`admitted`, `missed` or `rejected`), and nullable
`reason`, `session_id` and `turn_id`. A cursor belongs to that Environment and
activation; it cannot be reused for another activation.

Human-session equivalents are available under
`/api/projects/{project}/environments/{environment}/triggers`.
Agent Delete, Restore and retained status use separate human management routes;
see [Agent lifecycle](/docs/reference/api/agents).

### Deployment uploads

Upload planning returns the bundle digest and missing object descriptors
(`digest`, `size_bytes`, `media_type`). Send each object's bytes to the authenticated
PUT route on the same Helmr origin, with the descriptor's exact `Content-Length`
and `Content-Type`, then finalize the bundle. Each PUT checks current Environment
publish permission and membership in an admitted bundle. A `204` acknowledges
quarantine storage; finalization separately verifies and publishes the deployment.
No direct object-store URL or redirect is part of this protocol.

Objects and their total bundle remain limited to 4 GiB, with at most 513 objects.
Per-process and per-organization upload capacity, inactivity and maximum transfer
duration are explicit [operator settings](/docs/reference/env#control-plane).
They apply equally to Managed Cloud and self-hosted installations. The CLI waits
on `429` according to `Retry-After`, within its configurable per-object deadline,
and reconciles a lost success response through upload planning. Other interrupted
uploads can be rerun with the same bundle. Client and ingress limits can end a
transfer before its server deadline; configure and qualify the complete path.
Quarantined inputs expire under the AWS recipe's one-day lifecycle
(and noncurrent versions after one day); S3 applies expiration asynchronously.
Rerun upload planning if an unfinalized input has expired.
