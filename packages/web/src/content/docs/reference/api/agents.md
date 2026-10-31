---
title: Agent lifecycle
description: Inspect retained Agent identities and preview, delete or restore them with a human session.
sidebarLabel: Agent lifecycle
---

# Agent lifecycle

These management routes use a saved human session under
`/api/projects/{project}/environments/{environment}`. They are separate from
`/v1/agents`, which reads Deployment definitions. Delete requires `agents.delete`; Restore requires `agents.restore` on the target Agent; workload API keys and
Agent runtimes cannot perform them. `{agent}` is the identifier declared in code.

| Method | Path suffix | Purpose |
| --- | --- | --- |
| `GET` | `/agents?view=all` | List all visible retained identities, including deleted Agents. |
| `GET` | `/agents?view=deleted` | List visible deleted identities. |
| `GET` | `/agents/{agent}/status` | Read retained identity status and its revision. |
| `DELETE` | `/agents/{agent}?dry_run=true` | Preview deletion without changes. |
| `DELETE` | `/agents/{agent}` | Apply deletion. |
| `POST` | `/agents/{agent}/restore` | Preview or apply restoration. |

Identity lists accept `limit` (1–100, default 50) and `cursor` and return `agents`
and optional `next_cursor`. Keep the same view and Environment when continuing a
cursor. Omitting `view` reads definitions in the selected Deployment instead.
Status accepts no query parameters. Reads remain subject to the caller's surviving
Agent access after deletion.

## Status and revision

Status and mutation responses include:

| Field | Meaning |
| --- | --- |
| `agent` | Stable identifier within this Environment. |
| `current_deployment_id` | Current Deployment ID, or `null`. |
| `defined_in_current` | Whether that Deployment defines the Agent. |
| `deleted_at` | Deletion timestamp, or `null` when not deleted. |
| `revision` | Positive revision for conditional management writes. |

Status and successful mutation responses return an `ETag` containing the quoted
revision, for example `"7"`. Apply requests require that exact quoted value in
`If-Match`. Previews do not require it. Deleting or restoring a changed identity
increments its revision. Applying the already-current state with a matching
revision returns `changed: false` without incrementing it. A stale revision still
fails, even if the requested state already holds.

## Preview and apply

DELETE has no body; use the boolean `dry_run` query parameter for previews.
Restore accepts an optional JSON body; `{"dry_run":true}` previews restoration,
and an omitted body or `{"dry_run":false}` applies it. Restore accepts no query
parameters. Apply is conditional on `If-Match`, not an `idempotency_key`.

Both operations return HTTP 200 with status plus:

- `can_apply`: whether the inspected prerequisites allow the operation;
- `changed`: whether this request changed the deletion state (false for previews);
- `blockers`: entries with a `code`;
- `effects`: counts of `sessions_cancelled`, `selected_agent_edges_removed`,
  `ineffective_assignments_removed`, `mixed_assignments_preserved` and
  `trigger_activations_sealed`, plus nullable `slack_connection_disconnected`;
- `warnings`: explanatory strings for the operator, including access loss and
  the limits of Restore.

A deletion preview returns HTTP 200 even when `can_apply` is false. The only
blocker code is `defined_in_current`; resolve it by promoting a Deployment
without the Agent. Deleting cancels its Sessions and the Sessions they started,
including ones of other Agents, with no separate Session permission.
`sessions_cancelled` counts them; their execution cleanup continues afterward.

Preview effects describe the proposed cleanup. Applying rechecks authorization,
revision and dependencies; a preview is not a reservation. Refresh after a
conflict rather than applying an old preview blindly. See
[Deleting and restoring an Agent](/docs/concepts/agents#deleting-and-restoring-an-agent)
for history preservation, access cleanup, Slack disconnection and restoration.

## Errors

| HTTP status | Code | Next step |
| --- | --- | --- |
| 428 | `revision_required` | Read status or preview and supply its revision in `If-Match`. |
| 412 | `stale_revision` | Refresh the preview before applying again. |
| 409 | `agent_deletion_blocked` | Resolve blockers in `error.details.preview`. |
| 409 | `agent_dependencies_changed` | Refresh the preview after concurrent dependency changes. |
| 403 | `permission_required` | Both management permissions are required. |
| 404 | `not_found` | The identity is unknown or not visible to the caller. |

Malformed revisions or unsupported parameters return 400. Promotion or execution
against a deleted Agent returns 409 `agent_deleted` after permission checks.
Promotion includes `error.details.conflicts.agents` (up to 20 identifiers) and
`count` (the total number); restore those identities before promotion.
