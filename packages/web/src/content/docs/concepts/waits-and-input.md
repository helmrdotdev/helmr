---
title: Waits and input
description: Durable suspension with Actor input, Tokens, and timers.
sidebarLabel: Waits and input
---

# Waits and input

Helmr exposes three durable waiting patterns:

| Need | Primitive |
| --- | --- |
| Continuing commands, corrections, and progressive output | Actor Session |
| One external result such as an approval or callback | Token |
| Resume after a duration or timestamp | Timer |

Actor input is an ordered log owned by one Session. The Actor calls
`session.input.receive()` and external callers append JSON with an optional
idempotency key. Output is a separate ordered log with independent readers.
This is the right shape for a conversation or a workflow that can be steered
more than once.

A Token represents one pending value. `tokens.create()` returns the Token plus
a callback URL and public access credential. The owning Run waits with
`token.wait()`, optionally validating the completion through a Standard Schema.
Completed, cancelled, and expired are terminal Token states. The narrow public
completion capability is preferable when an outside user should not gain
access to an Actor channel.

Timers park only on time:

```ts
import { timers } from "@helmr/sdk"

await timers.waitFor("15m")
await timers.waitUntil(new Date("2026-08-10T09:00:00Z"))
```

A waiting Run has released active execution but retains durable intent to
resume. Set application-level timeouts for input and Tokens, and handle
`wait_timeout`, closed Sessions, cancelled Tokens, and expired Tokens as normal
branches. Avoid sending secret data through input, output, Token results, or
wait metadata.
