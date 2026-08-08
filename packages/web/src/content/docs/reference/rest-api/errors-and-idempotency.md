---
title: Errors and idempotency
description: Common REST error envelope and safe write retries.
sidebarLabel: Errors and idempotency
---

# Errors and idempotency

Helmr-owned HTTP errors use this envelope:

```json
{
  "error": {
    "code": "not_found",
    "message": "resource not found",
    "details": {}
  }
}
```

`code` and `message` are strings. `details` is an optional JSON object. Use the
HTTP status and stable `code` for program flow; do not parse `message`. The SDK
surfaces these as `APIError` values with `code`, optional `requestId`, and
optional `details`.

Write request bodies use `idempotency_key` where the endpoint supports stable
retries, including Task/Actor starts, Session input/close, Workspace creation,
exec/deletion, Secret changes, Token changes, and Deployment creation. SDK
request objects use `idempotencyKey`.

Reuse a key only for retries of the same logical operation. A replay can return
the accepted result; reusing a key with different canonical input can return a
conflict. Whether the field is optional or required is endpoint-specific—for
example Workspace exec requires it, while many creates generate or accept one.
