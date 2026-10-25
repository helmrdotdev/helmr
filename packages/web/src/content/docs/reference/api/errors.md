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

Turn errors and Computer diagnostics are separate from the HTTP envelope. Use
resource status for lifecycle decisions and keep a fallback for unfamiliar error
codes. A failure record does not prove physical stopping has converged.

Supported writes use `idempotency_key`, including Agent starts, Session input and
controls, Computer creation/exec/deletion, Secret changes and Deployment promotion.
SDK requests use `idempotencyKey`. Exact ask responses instead carry `answer_id`
(`answerId` in the SDK), paired with the same answer on retry.

Reuse a key only for retries of the same logical operation. A replay can return
the accepted result; reusing a key with different canonical input can return a
conflict. Whether the field is optional or required is endpoint-specific—for
example Computer exec requires it, while many creates generate or accept one.

Agent Delete and Restore use revision preconditions instead of idempotency keys.
A missing `If-Match` returns 428 `revision_required`; a stale one returns 412
`stale_revision`. Blocked deletion returns 409 `agent_deletion_blocked` with a
preview in `error.details.preview`. Promotion or execution against a deleted
Agent returns 409 `agent_deleted`. See [Agent lifecycle](/docs/reference/api/agents).
