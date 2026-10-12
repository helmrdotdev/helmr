---
title: Pagination
description: Opaque collection cursors, Session event sequences and Command log cursors.
---

# Pagination

## Collections

Paginated `/v1` collections accept an opaque `cursor` and a `limit`. The
response names its resources and includes `next_cursor` when more remain:

```json
{ "sessions": [], "next_cursor": "opaque-value" }
```

Pass `next_cursor` unchanged as `cursor` in the next request. A response without
`next_cursor` ends the traversal. Do not construct cursors or use them as IDs.

| Collection | Envelope field | `limit` |
| --- | --- | --- |
| `GET /v1/sessions` | `sessions` | 1–100, default 50 |
| `GET /v1/sessions/{sessionID}/turns` | `turns` | 1–100, default 50 |
| `GET /v1/sessions/{sessionID}/turns/{turnID}/asks` | `asks` | 1–100, default 50 |
| `GET /v1/agents` | `agents` | 1–100, default 50 |
| `GET /v1/templates` | `templates` | 1–100, default 50 |
| `GET /v1/computers` | `computers` | 1–100, default 50 |
| `GET /v1/computers/{computerID}/sessions` | `sessions` | 1–100, default 50 |
| `GET /v1/computers/{computerID}/commands` | `commands` | 1–100, default 50 |
| `GET /v1/schedules` | `schedules` | 1–100, default 50 |
| `GET /v1/secrets` | `secrets` | 1–100, default 50 |
| `GET /v1/deployments` | `deployments` | 1–100, default 50 |

Agent and Template pages also return `deployment_id`. A resource cursor keeps the
Deployment its first page selected, even if a newer Deployment is promoted.

Exact lookups are not paged: `key` on `/v1/computers`, `name` on
`/v1/secrets`, and `agent` with `key` on `/v1/sessions`. Do not send
`cursor` or `limit` with them; the SDK rejects the combination.

In the SDK, list methods return `{ items, nextCursor? }`; Agent/Template pages also
include `deploymentId`. See [Client](/docs/reference/sdk/client).

## Session events

`GET /v1/sessions/{sessionID}/events` uses durable integer sequences instead of
cursors.

| Parameter | Description |
| --- | --- |
| `after` | Nonnegative integer. Returns records with a higher sequence. Default `0`. |
| `limit` | 1–1000. Default 100. |

The response contains `records`, `next_after`, `has_more` and
`retained_after`. Store a record's sequence after processing it and pass it as
`after` to resume. An empty page or `has_more: false` does not mean the Turn
has finished; new events can arrive later. Event kinds are listed in
[Session events](/docs/reference/events).

## Command logs

`GET /v1/commands/{commandID}/logs` uses its own opaque cursor and returns an
`output_state` of `open`, `closed` or `unavailable`. A page can be empty while
the Command is running or output delivery is pending; check the Command outcome
and `output_state` separately.

Session and Schedule cursors are bound to their scope and filters. Computer
Sessions and Commands cursors are also bound to their collection kind. These
Computer collections show current usage and pending cleanup, not historical work.
