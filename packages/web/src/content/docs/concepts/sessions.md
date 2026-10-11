---
title: Sessions
description: Continuing work with pinned code, serial Turns and explicit controls.
---

# Sessions

A Session is one continuing work context for an Agent. It has a stable identity,
a pinned Deployment and an actual Computer. It runs optional setup before serving
its accepted Turns in FIFO order. Different Sessions can share a Computer while
retaining separate conversation identities.

`client.agents.start` returns the Session and first Turn. An optional Session key
selects a continuing conversation. A separate idempotency key identifies one
request, including an uncertain retry. Reusing a Session does not change its
pinned code when a new Deployment is promoted.

`session.enqueue(input)` always admits new FIFO work. `session.send(data)` steers
active work or enqueues when idle. Exact `session.turn(id).send(data)` targets
that Turn only. Acceptance does not establish callback delivery or application.

A safely converged application failure leaves the Session open for later input.
Unexpected execution loss or unsuccessful convergence holds dispatch. Healthy
hibernation preserves setup and native state. If no valid continuation remains,
explicit recovery can reconstruct setup from committed disk; it never replays an
interrupted input automatically.

| Control | Effect |
| --- | --- |
| Interrupt | Hold this Session and owned descendants, preserving queued work. |
| Resume | Release one exact authorized hold; interrupted inputs remain interrupted. |
| Close | Reject ordinary new admission and drain accepted work; existing holds remain. |
| Cancel | Stop active work and cancel queued work throughout the owned tree. |

Controls return acceptance receipts. Physical stopping can still be pending.
Inspect effective holds and their owning Session before resuming. Independent
Sessions are unaffected by controls on the Session that requested them.

Read the retained timeline with `session.events.list` or `.stream` and reconnect
from its durable sequence. Output EOF and page boundaries do not close a Session
or complete a Turn. See [Turn outcomes](/docs/concepts/turns).
