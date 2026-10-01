---
title: Logger and metadata
description: Emit structured logs and mutate Run metadata.
---

# Logger and metadata

`logger.debug`, `info`, `warn`, and `error` accept a message and optional
JSON-valued attributes. Each returns `Promise<void>`.

```ts
await logger.info("processing", { itemId: "item_123" })
```

`metadata.set(key, value)`, `metadata.patch(values)`, and
`metadata.increment(key, amount = 1)` mutate current Run metadata and also
return `Promise<void>`. Await these acknowledged runtime operations before entering
a managed wait or after it resolves. A concurrent metadata mutation while the Run
is waiting rejects that operation without cancelling the pending wait.

Structured logs appear as `kind: "structured"` records with level, message,
attributes, Run ID, attempt number, and timestamp. Stdout and stderr are
separate base64-encoded stream records.
