---
title: Turns
description: One admitted input, its output and its terminal outcome.
---

# Turns

A Turn is one input admitted to a [Session](/docs/concepts/sessions). It has
its own ID and a sequence number within the Session. Only one Turn runs at a
time in a Session, and Helmr calls the Agent's `turn` handler once for each
Turn.

## Status

| Status | Meaning |
| --- | --- |
| `queued` | Admitted and waiting to run. |
| `running` | The `turn` handler is processing. |
| `finalizing` | The handler returned; Helmr is finishing outstanding work and saving the Computer's disk. |
| `completed` | The handler succeeded and the post-turn disk save is stored durably. |
| `failed` | The handler threw, returned an invalid result or broke the Turn contract. |
| `interrupted` | An interrupt, `maxTurnDuration`, or process or host loss stopped the Turn before it completed. |
| `cancelled` | Cancelling the Session cancelled this Turn. |

`completed`, `failed`, `interrupted` and `cancelled` are terminal.

## Output, response and result

A Turn produces three separate things:

| | Written with | Purpose |
| --- | --- | --- |
| Output | `turn.output.write(value)`, `turn.output.pipe(values)` | Human-facing progress, readable while the Turn runs. |
| Response | `turn.respond(value)` | An optional human-facing answer, delivered with a successful outcome. |
| Result | The handler's return value | A JSON machine result for the caller. |

```ts
async turn(turn) {
  await turn.output.write("Checking the build")
  await turn.respond("The build is green.")
  return { status: "green" }
}
```

Output can be read as soon as it is written, before the Turn finishes. A
handler's return, the harness's final text or the end of output does not by
itself mean the Turn is `completed`.

## Finalization

When the handler returns, the Turn stops accepting new messages and questions
and enters `finalizing`. Helmr then:

1. waits for output writes, message callbacks and questions that already
   started;
2. lets the harness's native work settle;
3. saves the Computer's disk and waits until the save is stored durably.

Only then does the Turn become `completed`. Finish your own writes and flush
buffers before returning so they are part of the save. If the save must be
retried, Helmr retries the save; it does not rerun the handler. See
[Durability](/docs/concepts/durability) for what the save does and does not
guarantee.

## Observing the outcome

Wait for a Turn with `turn.wait()` or `helmr session turn wait`, or read it with
`turn.get()` or `helmr session turn get`. The outcome includes the status, the
result, the response and an error code for failures. A wait with a timeout can
return `timeout`; that ends the wait only, not the Turn. Use
[Turn interrupt](/docs/concepts/sessions#controls) to stop only the running Turn,
or Session interrupt to also hold queued work.

Stored inputs, results and responses are kept for the Environment's history
retention period. After that, reads report that the payload has expired while
the Turn's identity and status remain.

## After a failure

A `failed` Turn normally leaves the Session open, and the next queued Turn
runs. If the harness's work cannot be stopped cleanly after the failure, the
Session is held instead.

An exact-Turn interrupt lets the next queued Turn run once processing has
stopped. Session interruption, `maxTurnDuration`, and process or host loss hold
the Session until someone resumes it. Helmr never replays an interrupted or
failed Turn; retrying business work and reconciling external effects belong to
the application.

## Related

- [Start and observe a Session](/docs/guides/start-and-observe-a-session)
- [Questions](/docs/concepts/questions)
- [Events](/docs/reference/events)
