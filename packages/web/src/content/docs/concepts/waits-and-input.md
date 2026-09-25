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

Actor work is FIFO within a Session. The Actor calls `session.receive()` for a
Turn; external callers use `enqueue` for new work or exact `turn.send` for live
interaction. `turn.onMessage` handles application-defined questions, approvals
and corrections. Output and lifecycle events share one ordered timeline.

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

A managed wait first retains the running environment for its hot-wait duration.
Once suspension becomes eligible, Helmr can checkpoint and release compute while
retaining durable intent to resume. Set application-level timeouts for input and Tokens, and handle
`wait_timeout`, a null receive when closing drains, cancelled Tokens, and expired Tokens as normal
branches. Avoid sending secret data through input, output, Token results, or
wait metadata.

## Hot waiting and response deadlines

`idleTimeout` determines when a managed wait becomes eligible for suspension.
The default comes from the owning Actor's `idleTimeout`, or 30 seconds for a Task.
An explicit wait option overrides that default. Supported durations are 1 ms to
1 hour.

`timeout` limits how long the operation waits for its answer. It continues to
elapse while execution is suspended. Token expiry belongs to the Token itself;
a particular Token waiter can have an earlier timeout. Reaching an idle deadline
does not complete or fail the Token or Turn. A Token wait within a Turn keeps that
Turn active; `session.receive()` waits for the next Turn after settlement.

Hot-wait duration is not a hard billing cap: checkpoint work takes time, and
resource lifecycle constraints affect when compute can actually be released.
