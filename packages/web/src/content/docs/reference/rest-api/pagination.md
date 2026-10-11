---
title: Pagination
description: Opaque collection cursors and durable Session event sequences.
---

# Pagination

Paginated `/v1` collections accept an opaque `cursor` and an endpoint-specific
`limit`. For example:

```json
{ "sessions": [], "next_cursor": "opaque-value" }
```

Pass `next_cursor` unchanged in the next request. Absence of that field ends the
collection traversal. Do not construct cursors or use them as resource IDs.
Definition cursors retain the selected Deployment snapshot.

SDK collection pages generally use `{ items, nextCursor? }`; Slack destinations
use `{ channels, nextCursor? }`. Most list limits range from 1 to 100. Exact
Computer-key or Agent-and-Session-key lookups cannot be combined with pagination
where their query types forbid it.

Session events instead use a nonnegative integer `after` and return
`records`, `next_after`, `has_more` and `retained_after`. The default event limit
is 100, with a maximum of 1000. Store a sequence after consuming its record.
An empty page or `has_more: false` does not establish Turn completion; new events
can arrive later. See [Session events](/docs/reference/session-events).

Computer Command logs use their own scoped cursor. An output page can be empty
while work is running or while delivery is pending; observe the Command outcome
and output state separately.

Console `GET /api/projects` also uses an opaque cursor, with default limit 50 and
maximum 100. Fetch a project by ID or slug to load its Environments. UUID-shaped
references are interpreted as IDs, so project slugs cannot use UUID syntax.
