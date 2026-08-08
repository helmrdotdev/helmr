---
title: helmr token
description: Inspect, complete, and cancel external completion Tokens.
sidebarLabel: token
---

# `helmr token`

```text
helmr token get TOKEN [--json]
helmr token complete TOKEN --data-json JSON [--idempotency-key KEY] [--json]
helmr token cancel TOKEN [--idempotency-key KEY] [--json]
```

All commands accept project/environment scope. Completion requires one JSON
value. Idempotency keys make completion or cancellation safe to retry. Token
creation is available through the SDK and REST API, not this CLI group.
