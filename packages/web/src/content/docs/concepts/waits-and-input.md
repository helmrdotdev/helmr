---
title: Waits and input
description: Queue new work, steer an active Turn or answer an attributable question.
---

# Waits and input

Choose the operation by the lifetime of the input:

| Need | Operation |
| --- | --- |
| New work, even while busy or held | `session.enqueue(input)` |
| Steer active work, or start work when idle | `session.send(data)` |
| Message exactly one Turn | `session.turn(turnId).send(data)` |
| Ask for a typed human answer | `await turn.ask({ prompt, answer })` |
| Observe an outcome | `turn.wait({ timeout })` |

Register live steering with `await turn.onMessage(handler)`. Callbacks execute
serially for that Turn. An accepted message can still await delivery; settling
work rejects new input rather than redirecting it to another Turn. Held Sessions
reject automatic send while retaining eligible explicit enqueues.

A question belongs to one Session and Turn and has its own ask ID. Clients read
its prompt and answer control and respond to that exact ask. The result contains
the answer and authenticated responder. A message is not a substitute for the
ask response API. See [Questions](/docs/reference/sdk/questions).

Await questions and callbacks that your work starts. Handler return closes new
input admission; already-admitted questions may still resolve during drainage.
Without a chosen Turn duration limit, that drainage may wait for a human.

Use ordinary cancellable JavaScript or harness facilities for short delays.
Compute release depends on all work resident on the Computer becoming eligible;
a pending question or arbitrary promise does not independently request release.
Healthy release preserves setup and native process state.

Client observation timeouts do not stop execution. To stop active work, interrupt
the Session; inspect the resulting hold and explicitly resume it when appropriate.
Keep credentials out of inputs, answers and recorded content.
