---
title: Tokens
description: Create and wait for externally completable values.
---

# Tokens

Runtime code creates an external completion Token with
`tokens.create({ timeout?, metadata?, tags?, idempotencyKey? })`. The result is
a pending Token plus `callbackUrl`, `publicAccessToken`, and `wait()`.

```ts
const approval = await tokens.create({ timeout: "30m" })
const value = await approval
  .wait({ timeout: "35m", schema })
  .unwrap()
```

`tokens.ref(id)` creates a runtime wait reference. `wait` returns a discriminated
result; `unwrap()` throws `wait_timeout`, `token_cancelled`, or `token_expired`.
Token statuses are `pending`, `completed`, `cancelled`, and `expired`.

External code uses `client.tokens.create`, `retrieve`, `list`, `complete`, and
`cancel`. Completion takes `{ result, idempotencyKey? }`; cancellation takes an
optional idempotency key. Treat the callback URL and public access token as
credentials.
