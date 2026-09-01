---
title: Pagination
description: Cursor pagination for public collection and telemetry endpoints.
sidebarLabel: Pagination
---

# Pagination

Paginated `/v1` collections accept an opaque `cursor` and an endpoint-specific
`limit`. Responses include the plural collection and may include
`next_cursor`:

```json
{
  "runs": [],
  "next_cursor": "opaque-value"
}
```

Pass `next_cursor` unchanged as the next request's `cursor`. Absence of
`next_cursor` ends the traversal. Do not decode, compare, persist as a resource
ID, or construct cursors.

The SDK maps a collection page to `{ items, nextCursor? }`. Most SDK list
limits validate in the inclusive range 1–100. Exact key filters such as a
Workspace key, Actor key, or Schedule task ID cannot be combined with cursor
pagination where their query type forbids it.

Run logs and Run events also use finite cursor pages. Following is client-side
polling from the returned cursor, not an infinite response stream. Actor
Session output uses a different durable integer sequence contract:
`after`, `next_after`, and `has_more`.

The session-authenticated Console transport also applies this opaque cursor
contract to `GET /api/projects`, with a default limit of 50 and maximum of 100.
That endpoint returns project summaries; fetch one project by ID or slug to
load its Environments. UUID-shaped project references are interpreted as IDs,
so project slugs cannot use UUID syntax.
